// Package clipboard reads copied files and images from the local system clipboard.
package clipboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var (
	// ErrEmpty means the clipboard holds neither copied files nor an image.
	ErrEmpty = errors.New("no copied files or image on the clipboard")
	// ErrUnreadable means the clipboard tool failed or did not answer in time.
	ErrUnreadable = errors.New("clipboard did not answer")
)

const (
	// toolTimeout bounds one xclip or wl-paste call. On GNOME, wl-paste waits
	// for a popup to take focus, which GNOME sometimes never grants.
	toolTimeout = 3 * time.Second
	// powershellTimeout covers powershell.exe starting cold from WSL.
	powershellTimeout = 10 * time.Second
)

// Read returns copied files, or the copied image saved to a temp PNG; image
// reports that temp file, which the caller removes. Plain text is never read
// as paths, and a cut selection is read without moving files.
// WSL reads the Windows clipboard through one PowerShell call; Linux uses
// xclip when an X display is up, and wl-paste otherwise.
func Read(ctx context.Context) (paths []string, image bool, err error) {
	if isWSL() {
		return readWSL(ctx)
	}
	return readNative(ctx)
}

func isWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	release, err := os.ReadFile("/proc/version")
	return err == nil && bytes.Contains(bytes.ToLower(release), []byte("microsoft"))
}

func imagePath() string {
	b := make([]byte, 8)
	rand.Read(b)
	return filepath.Join(os.TempDir(), "agency-paste-"+hex.EncodeToString(b)+".png")
}

// nativeTool builds the command that prints one clipboard target, TARGETS
// listing the offered types. xclip reads through Xwayland and needs no focus,
// so it wins over wl-paste whenever it can reach an X display.
func nativeTool() func(context.Context, string) *exec.Cmd {
	if _, err := exec.LookPath("xclip"); (err == nil && os.Getenv("DISPLAY") != "") || os.Getenv("WAYLAND_DISPLAY") == "" {
		return func(ctx context.Context, target string) *exec.Cmd {
			return exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", target, "-o")
		}
	}
	return func(ctx context.Context, target string) *exec.Cmd {
		if target == "TARGETS" {
			return exec.CommandContext(ctx, "wl-paste", "--list-types")
		}
		return exec.CommandContext(ctx, "wl-paste", "--type", target, "--no-newline")
	}
}

func readNative(ctx context.Context) ([]string, bool, error) {
	tool := nativeTool()
	read := func(target string) ([]byte, error) { return output(ctx, tool, target) }
	types, err := read("TARGETS")
	if err != nil {
		return nil, false, err
	}
	offered := strings.Fields(string(types))
	if paths, err := readFileList(offered, read); paths != nil || err != nil {
		return paths, false, err
	}
	if !slices.Contains(offered, "image/png") {
		return nil, false, ErrEmpty
	}
	data, err := read("image/png")
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return nil, false, ErrEmpty
	}
	path := imagePath()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, false, err
	}
	return []string{path}, true, nil
}

// output runs one clipboard read. wl-paste and xclip report an empty
// clipboard as an error, which becomes ErrEmpty.
func output(ctx context.Context, tool func(context.Context, string) *exec.Cmd, target string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	cmd := tool(ctx, target)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("%s is not installed", cmd.Args[0])
	}
	message := strings.TrimSpace(stderr.String())
	if strings.Contains(message, "Nothing is copied") || strings.Contains(message, "target TARGETS not available") {
		return nil, ErrEmpty
	}
	return nil, fmt.Errorf("%w: %s %s: %v: %s", ErrUnreadable, cmd.Args[0], target, err, message)
}

func readWSL(ctx context.Context) ([]string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, powershellTimeout)
	defer cancel()
	path := imagePath()
	winPath, err := exec.CommandContext(ctx, "wslpath", "-w", path).Output()
	if err != nil {
		return nil, false, fmt.Errorf("wslpath: %w", err)
	}
	script := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
if ([System.Windows.Forms.Clipboard]::ContainsFileDropList()) {
  $files = @([System.Windows.Forms.Clipboard]::GetFileDropList() | ForEach-Object { [string]$_ })
  ConvertTo-Json -InputObject $files -Compress
  exit 0
}
$img = [System.Windows.Forms.Clipboard]::GetImage()
if ($img -eq $null) { exit 3 }
$img.Save('%s', [System.Drawing.Imaging.ImageFormat]::Png)
'image'`, strings.TrimSpace(string(winPath)))
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-STA", "-Command", script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			return nil, false, ErrEmpty
		}
		return nil, false, fmt.Errorf("%w: powershell: %v: %s", ErrUnreadable, err, strings.TrimSpace(stderr.String()))
	}
	if strings.TrimSpace(string(data)) == "image" {
		return []string{path}, true, nil
	}
	paths, err := windowsPaths(ctx, data)
	return paths, false, err
}
