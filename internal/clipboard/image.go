// Package clipboard reads an image off the local system clipboard.
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
	"strings"
	"time"
)

// ErrNoImage means the clipboard holds no image.
var ErrNoImage = errors.New("no image on the clipboard")

const readTimeout = 5 * time.Second

// ReadImage writes the clipboard image to a temp PNG and returns its path.
// WSL reads the Windows clipboard through PowerShell; Wayland uses wl-paste;
// X11 uses xclip.
func ReadImage(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	path := filepath.Join(os.TempDir(), "agency-paste-"+randomHex()+".png")
	switch {
	case isWSL():
		return readWSL(ctx, path)
	case os.Getenv("WAYLAND_DISPLAY") != "":
		return readWayland(ctx, path)
	default:
		return readX11(ctx, path)
	}
}

func randomHex() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func isWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	release, err := os.ReadFile("/proc/version")
	return err == nil && bytes.Contains(bytes.ToLower(release), []byte("microsoft"))
}

func readWSL(ctx context.Context, path string) (string, error) {
	winPath, err := exec.CommandContext(ctx, "wslpath", "-w", path).Output()
	if err != nil {
		return "", fmt.Errorf("wslpath: %w", err)
	}
	script := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms
$img = [System.Windows.Forms.Clipboard]::GetImage()
if ($img -eq $null) { exit 3 }
$img.Save('%s', [System.Drawing.Imaging.ImageFormat]::Png)`, strings.TrimSpace(string(winPath)))
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-STA", "-Command", script)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			return "", ErrNoImage
		}
		return "", fmt.Errorf("powershell clipboard: %w", err)
	}
	return path, nil
}

func readWayland(ctx context.Context, path string) (string, error) {
	types, err := exec.CommandContext(ctx, "wl-paste", "--list-types").Output()
	if err != nil || !bytes.Contains(types, []byte("image/png")) {
		return "", ErrNoImage
	}
	return pipeToFile(exec.CommandContext(ctx, "wl-paste", "--type", "image/png", "--no-newline"), path)
}

func readX11(ctx context.Context, path string) (string, error) {
	targets, err := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o").Output()
	if err != nil || !bytes.Contains(targets, []byte("image/png")) {
		return "", ErrNoImage
	}
	return pipeToFile(exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "image/png", "-o"), path)
}

func pipeToFile(cmd *exec.Cmd, path string) (string, error) {
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", cmd.Args[0], err)
	}
	if len(out) == 0 {
		return "", ErrNoImage
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
