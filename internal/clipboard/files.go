package clipboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// ErrNoFiles means the clipboard does not advertise a file list.
var ErrNoFiles = errors.New("no files on the clipboard")

// ReadFiles reads copied files, never paths inferred from plain text.
// A cut selection is read without moving files or changing the clipboard.
func ReadFiles(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	if isWSL() {
		return readFilesWSL(ctx)
	}
	var types []byte
	var err error
	var read func(string) ([]byte, error)
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		types, err = exec.CommandContext(ctx, "wl-paste", "--list-types").Output()
		read = func(mime string) ([]byte, error) {
			return exec.CommandContext(ctx, "wl-paste", "--type", mime, "--no-newline").Output()
		}
	} else {
		types, err = exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o").Output()
		read = func(mime string) ([]byte, error) {
			return exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", mime, "-o").Output()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("reading clipboard file types: %w", err)
	}
	return readFileList(types, read)
}

func readFileList(types []byte, read func(string) ([]byte, error)) ([]string, error) {
	for _, mime := range []string{"x-special/gnome-copied-files", "text/uri-list"} {
		for _, offered := range strings.Fields(string(types)) {
			if offered != mime {
				continue
			}
			data, err := read(mime)
			if err != nil {
				return nil, fmt.Errorf("reading clipboard files: %w", err)
			}
			return parseFileList(mime, string(data))
		}
	}
	return nil, ErrNoFiles
}

func parseFileList(mime, data string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	if mime == "x-special/gnome-copied-files" {
		if lines[0] != "copy" && lines[0] != "cut" {
			return nil, fmt.Errorf("clipboard file list must start with copy or cut")
		}
		lines = lines[1:]
	}
	var paths []string
	for _, line := range lines {
		if line == "" || (mime == "text/uri-list" && strings.HasPrefix(line, "#")) {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Scheme != "file" || (u.Host != "" && !strings.EqualFold(u.Host, "localhost")) || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !filepath.IsAbs(u.Path) || strings.HasPrefix(u.Path, "//") {
			return nil, fmt.Errorf("clipboard contains a nonlocal or invalid file URI: %q", line)
		}
		if strings.ContainsFunc(u.Path, unicode.IsControl) {
			return nil, fmt.Errorf("clipboard file path contains control characters")
		}
		paths = append(paths, u.Path)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("clipboard file list is empty")
	}
	return paths, nil
}

func readFilesWSL(ctx context.Context) ([]string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
if (-not [System.Windows.Forms.Clipboard]::ContainsFileDropList()) { exit 3 }
$files = @([System.Windows.Forms.Clipboard]::GetFileDropList() | ForEach-Object { [string]$_ })
ConvertTo-Json -InputObject $files -Compress`
	data, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-STA", "-Command", script).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			return nil, ErrNoFiles
		}
		return nil, fmt.Errorf("powershell clipboard files: %w", err)
	}
	var files []string
	if err := json.Unmarshal(data, &files); err != nil {
		return nil, fmt.Errorf("powershell clipboard files: %w", err)
	}
	var paths []string
	for _, file := range files {
		if len(file) < 3 || !((file[0] >= 'A' && file[0] <= 'Z') || (file[0] >= 'a' && file[0] <= 'z')) || file[1] != ':' || (file[2] != '\\' && file[2] != '/') || strings.ContainsFunc(file, unicode.IsControl) {
			return nil, fmt.Errorf("clipboard contains a nonlocal or invalid Windows file path: %q", file)
		}
		path, err := exec.CommandContext(ctx, "wslpath", "-u", file).Output()
		if err != nil {
			return nil, fmt.Errorf("wslpath for %q: %w", file, err)
		}
		paths = append(paths, strings.TrimRight(string(path), "\r\n"))
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("clipboard file list is empty")
	}
	return paths, nil
}
