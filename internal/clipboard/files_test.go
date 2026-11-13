package clipboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseFileList(t *testing.T) {
	for _, tt := range []struct {
		mime string
		data string
		want []string
	}{
		{"text/uri-list", "# files\r\nfile:///tmp/report%20one.txt\r\nfile://localhost/tmp/second%27s.txt\r\n", []string{"/tmp/report one.txt", "/tmp/second's.txt"}},
		{"x-special/gnome-copied-files", "copy\nfile:///tmp/one\nfile:///tmp/two", []string{"/tmp/one", "/tmp/two"}},
		{"x-special/gnome-copied-files", "cut\nfile:///tmp/one", []string{"/tmp/one"}},
		{"text/uri-list", "file:///tmp/%C3%A6.txt\n", []string{"/tmp/æ.txt"}},
	} {
		got, err := parseFileList(tt.mime, tt.data)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("%q: got=%v err=%v want=%v", tt.data, got, err, tt.want)
		}
	}
}

func TestParseFileListRejectsNonlocalAndMalformed(t *testing.T) {
	for _, data := range []string{
		"", "# empty", "/tmp/file", "file:relative", "https://example.com/file", "smb://server/share/file",
		"file://server/tmp/file", "file://localhost:22/tmp/file", "file://user@localhost/tmp/file",
		"file:////server/share/file", "file:///tmp/%zz", "file:///tmp/file?query", "file:///tmp/file?",
		"file:///tmp/file#fragment", "file:///tmp/line%0Abreak", "file:///tmp/%00",
		"file:///tmp/one\nhttps://example.com/two",
	} {
		if _, err := parseFileList("text/uri-list", data); err == nil {
			t.Errorf("accepted %q", data)
		}
	}
	for _, data := range []string{"file:///tmp/one", "move\nfile:///tmp/one", "copy\n"} {
		if _, err := parseFileList("x-special/gnome-copied-files", data); err == nil {
			t.Errorf("accepted GNOME list %q", data)
		}
	}
}

func TestReadFileListIgnoresPlainText(t *testing.T) {
	read := func(string) ([]byte, error) {
		t.Fatal("plain text was requested as files")
		return nil, nil
	}
	for _, types := range []string{"text/plain", "text/plain;charset=utf-8\nimage/png", "application/octet-stream"} {
		if paths, err := readFileList(strings.Fields(types), read); paths != nil || err != nil {
			t.Fatal(paths, err)
		}
	}
}

func TestReadFileListPrefersGNOMEAndDoesNotFallbackOnError(t *testing.T) {
	var requested []string
	read := func(mime string) ([]byte, error) {
		requested = append(requested, mime)
		return []byte("cut\nfile:///tmp/copied"), nil
	}
	got, err := readFileList([]string{"text/uri-list", "x-special/gnome-copied-files", "text/plain"}, read)
	if err != nil || !reflect.DeepEqual(got, []string{"/tmp/copied"}) || !reflect.DeepEqual(requested, []string{"x-special/gnome-copied-files"}) {
		t.Fatal(got, err, requested)
	}
	failure := errors.New("clipboard read failed")
	_, err = readFileList([]string{"text/uri-list"}, func(string) ([]byte, error) { return nil, failure })
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	_, err = readFileList([]string{"x-special/gnome-copied-files", "text/uri-list"}, func(mime string) ([]byte, error) {
		if mime != "x-special/gnome-copied-files" {
			t.Fatal("malformed file selection fell back")
		}
		return []byte("broken"), nil
	})
	if err == nil {
		t.Fatal("accepted malformed file list")
	}
}

func clipboardTool(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestReadNativeClipboard(t *testing.T) {
	if isWSL() {
		t.Skip("native clipboard route")
	}
	for _, wayland := range []bool{true, false} {
		t.Run(map[bool]string{true: "wayland", false: "x11"}[wayland], func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			if wayland {
				t.Setenv("WAYLAND_DISPLAY", "test")
				clipboardTool(t, dir, "wl-paste", `case "$*" in
--list-types) printf 'text/plain\nx-special/gnome-copied-files\n' ;;
'--type x-special/gnome-copied-files --no-newline') printf 'cut\nfile:///tmp/report%%20space.txt' ;;
*) exit 4 ;;
esac`)
			} else {
				t.Setenv("WAYLAND_DISPLAY", "")
				clipboardTool(t, dir, "xclip", `case "$*" in
'-selection clipboard -t TARGETS -o') printf 'text/plain\ntext/uri-list\n' ;;
'-selection clipboard -t text/uri-list -o') printf 'file:///tmp/report%%20space.txt\r\n' ;;
*) exit 4 ;;
esac`)
			}
			got, image, err := Read(context.Background())
			if err != nil || image || !reflect.DeepEqual(got, []string{"/tmp/report space.txt"}) {
				t.Fatal(got, image, err)
			}
		})
	}
}

func TestReadImage(t *testing.T) {
	if isWSL() {
		t.Skip("native clipboard route")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "test")
	clipboardTool(t, dir, "wl-paste", `case "$*" in
--list-types) printf 'image/png\n' ;;
'--type image/png --no-newline') printf 'image fixture' ;;
*) exit 4 ;;
esac`)
	paths, image, err := Read(context.Background())
	if err != nil || !image || len(paths) != 1 {
		t.Fatal(paths, image, err)
	}
	defer os.Remove(paths[0])
	data, err := os.ReadFile(paths[0])
	if err != nil || string(data) != "image fixture" {
		t.Fatal(string(data), err)
	}
}

func TestReadPrefersXclipOnWayland(t *testing.T) {
	if isWSL() {
		t.Skip("native clipboard route")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "test")
	t.Setenv("DISPLAY", ":0")
	clipboardTool(t, dir, "wl-paste", "exit 9")
	clipboardTool(t, dir, "xclip", `case "$*" in
'-selection clipboard -t TARGETS -o') printf 'text/uri-list\n' ;;
'-selection clipboard -t text/uri-list -o') printf 'file:///tmp/one' ;;
*) exit 4 ;;
esac`)
	if got, _, err := Read(context.Background()); err != nil || !reflect.DeepEqual(got, []string{"/tmp/one"}) {
		t.Fatal(got, err)
	}
}

func TestReadEmptyAndUnreadable(t *testing.T) {
	if isWSL() {
		t.Skip("native clipboard route")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "test")
	for script, want := range map[string]error{
		`printf 'text/plain\n'`:                                    ErrEmpty,
		`echo "Nothing is copied" >&2; exit 1`:                     ErrEmpty,
		`echo "Failed to connect to a Wayland server" >&2; exit 1`: ErrUnreadable,
		`exec /bin/sleep 10`:                                       ErrUnreadable,
	} {
		clipboardTool(t, dir, "wl-paste", script)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, _, err := Read(ctx)
		cancel()
		if !errors.Is(err, want) {
			t.Fatalf("%s: %v", script, err)
		}
	}
	t.Setenv("WAYLAND_DISPLAY", "")
	if _, _, err := Read(context.Background()); err == nil || errors.Is(err, ErrUnreadable) || !strings.Contains(err.Error(), "xclip is not installed") {
		t.Fatal(err)
	}
	clipboardTool(t, dir, "xclip", `echo "Error: target TARGETS not available" >&2; exit 1`)
	if _, _, err := Read(context.Background()); !errors.Is(err, ErrEmpty) {
		t.Fatal(err)
	}
}

func wslTools(t *testing.T, powershell string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	clipboardTool(t, dir, "powershell.exe", powershell)
	clipboardTool(t, dir, "wslpath", `case "$1 $2" in
'-u C:\Users\Lemon\report space.txt') printf '/mnt/c/Users/Lemon/report space.txt\n' ;;
'-u D:\æ.txt') printf '/mnt/d/æ.txt\n' ;;
-w*) printf '\\\\wsl.localhost\\Ubuntu%s\n' "$2" ;;
*) exit 4 ;;
esac`)
	return dir
}

func TestReadWSL(t *testing.T) {
	dir := wslTools(t, `printf '%s' '["C:\\Users\\Lemon\\report space.txt","D:\\æ.txt"]'`)
	got, image, err := readWSL(context.Background())
	if err != nil || image || !reflect.DeepEqual(got, []string{"/mnt/c/Users/Lemon/report space.txt", "/mnt/d/æ.txt"}) {
		t.Fatal(got, image, err)
	}
	clipboardTool(t, dir, "powershell.exe", `printf 'image\r\n'`)
	if got, image, err := readWSL(context.Background()); err != nil || !image || len(got) != 1 || !strings.HasPrefix(got[0], os.TempDir()) {
		t.Fatal(got, image, err)
	}
	clipboardTool(t, dir, "powershell.exe", "exit 3")
	if _, _, err := readWSL(context.Background()); !errors.Is(err, ErrEmpty) {
		t.Fatal(err)
	}
	clipboardTool(t, dir, "powershell.exe", `echo "Requested Clipboard operation did not succeed." >&2; exit 1`)
	if _, _, err := readWSL(context.Background()); !errors.Is(err, ErrUnreadable) {
		t.Fatal(err)
	}
	for _, output := range []string{`broken`, `[]`, `["\\\\server\\share\\file"]`, `["relative.txt"]`, `["C:\\line\nfile"]`} {
		clipboardTool(t, dir, "powershell.exe", "printf '%s' '"+output+"'")
		if _, _, err := readWSL(context.Background()); err == nil || errors.Is(err, ErrUnreadable) {
			t.Fatalf("accepted %q: %v", output, err)
		}
	}
	clipboardTool(t, dir, "powershell.exe", `printf '%s' '["C:\\file.txt"]'`)
	if _, _, err := readWSL(context.Background()); err == nil || !strings.Contains(err.Error(), "wslpath") {
		t.Fatal(err)
	}
}

func TestReadWSLTimeout(t *testing.T) {
	wslTools(t, "exec /bin/sleep 10")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := readWSL(ctx); !errors.Is(err, ErrUnreadable) {
		t.Fatal(err)
	}
}
