package clipboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// readFileList reads the preferred file list type on offer, and returns no
// paths and no error when none is.
func readFileList(offered []string, read func(string) ([]byte, error)) ([]string, error) {
	for _, mime := range []string{"x-special/gnome-copied-files", "text/uri-list"} {
		if !slices.Contains(offered, mime) {
			continue
		}
		data, err := read(mime)
		if err != nil {
			return nil, fmt.Errorf("reading clipboard files: %w", err)
		}
		return parseFileList(mime, string(data))
	}
	return nil, nil
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

// windowsPaths converts PowerShell's JSON list of Windows paths to WSL paths.
func windowsPaths(ctx context.Context, data []byte) ([]string, error) {
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
