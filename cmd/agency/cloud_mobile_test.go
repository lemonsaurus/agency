package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/control"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProjects(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"b/two", "a/one", ".hidden/repo", "a/.hidden"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "a", "file"), nil, 0o600)
	os.Symlink(filepath.Join(root, "b", "two"), filepath.Join(root, "a", "link"))
	got, err := projects(root)
	want := []project{{"a/one", filepath.Join(root, "a", "one")}, {"b/two", filepath.Join(root, "b", "two")}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("projects=%v, %v", got, err)
	}
}

func TestFileRead(t *testing.T) {
	home := t.TempDir()
	for _, dir := range []string{"git/repo", ".agents/files", "git-other", "outside"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("hello\n\x00\xff")
	for _, path := range []string{"git/repo/y.kt", ".agents/files/note.md", "git-other/file", "outside/file"} {
		if err := os.WriteFile(filepath.Join(home, path), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{
		"git/repo/escape": "outside/file", "git/repo/escape-dir": "outside",
		"git/repo/allowed": ".agents/files/note.md", "git/repo/missing": "not-found",
	} {
		if err := os.Symlink(filepath.Join(home, target), filepath.Join(home, name)); err != nil {
			t.Fatal(err)
		}
	}
	oversize := filepath.Join(home, "git/repo/large")
	file, err := os.Create(oversize)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	for _, tt := range []struct {
		path string
		mime string
	}{
		{"git/repo/y.kt", "application/octet-stream"},
		{".agents/files/note.md", "text/markdown"},
		{"git/repo/allowed", "application/octet-stream"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			var output strings.Builder
			if err := fileRead(home, filepath.Join(home, tt.path), &output); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(output.String(), "\n")
			if len(lines) != 3 || lines[2] != "" {
				t.Fatalf("wrong framing: %q", output.String())
			}
			var metadata struct {
				Name string `json:"name"`
				Size int    `json:"size"`
				MIME string `json:"mime"`
			}
			if err := json.Unmarshal([]byte(lines[0]), &metadata); err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.StdEncoding.DecodeString(lines[1])
			if err != nil || string(decoded) != string(body) || metadata.Name != filepath.Base(tt.path) || metadata.Size != len(body) || metadata.MIME != tt.mime {
				t.Fatalf("metadata=%+v decoded=%q err=%v", metadata, decoded, err)
			}
		})
	}
	for _, path := range []string{
		filepath.Join(home, "outside/file"), filepath.Join(home, "git-other/file"),
		filepath.Join(home, "git/repo/escape"), filepath.Join(home, "git/repo/escape-dir/file"),
		filepath.Join(home, "git/repo/missing"), filepath.Join(home, "git/repo"),
		home + "/git/../outside/file", "git/repo/y.kt", oversize,
	} {
		t.Run("reject "+path, func(t *testing.T) {
			var output strings.Builder
			if err := fileRead(home, path, &output); err == nil || output.Len() != 0 {
				t.Fatalf("accepted %q or wrote partial output: %v", path, err)
			}
		})
	}
	for ext, mime := range map[string]string{
		"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif",
		"webp": "image/webp", "svg": "image/svg+xml", "md": "text/markdown", "txt": "text/plain", "json": "application/json",
	} {
		path := filepath.Join(home, ".agents/files/file."+strings.ToUpper(ext))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var output strings.Builder
		if err := fileRead(home, path, &output); err != nil || !strings.Contains(output.String(), `"mime":"`+mime+`"`) || !strings.HasSuffix(output.String(), "\n\n") {
			t.Fatalf("mime for %s: %q, %v", ext, output.String(), err)
		}
	}
	if err := os.Truncate(oversize, maxFileBytes); err != nil {
		t.Fatal(err)
	}
	if err := fileRead(home, oversize, io.Discard); err != nil {
		t.Fatalf("5 MiB boundary: %v", err)
	}
}

func TestVoiceToken(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"success", 200, fmt.Sprintf(`{"value":"ephemeral","expires_at":%d,"session":{"model":"gpt-realtime"}}`, time.Now().Unix()+60), false},
		{"provider error", 401, `secret must never reach stderr`, true},
		{"redirect", 302, `{}`, true},
		{"malformed", 200, `nope`, true},
		{"missing token", 200, `{}`, true},
		{"expired", 200, `{"value":"ephemeral","expires_at":1}`, true},
		{"long lived key", 200, fmt.Sprintf(`{"value":"private-key","expires_at":%d}`, time.Now().Unix()+60), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != "POST" || r.URL.String() != "https://api.openai.com/v1/realtime/client_secrets" || r.Header.Get("Authorization") != "Bearer private-key" {
					t.Fatal("wrong request")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"session":{"type":"realtime","model":"gpt-realtime"}}` {
					t.Fatalf("body=%s", body)
				}
				return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body))}, nil
			})}
			token, err := mintVoiceToken(context.Background(), client, "private-key")
			if (err != nil) != tt.wantErr {
				t.Fatalf("token=%v err=%v", token, err)
			}
			if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private-key")) {
				t.Fatal("credential leaked")
			}
			if err == nil {
				encoded, _ := json.Marshal(token)
				if strings.Contains(string(encoded), "session") || token.Value != "ephemeral" {
					t.Fatal("wrong token output")
				}
			}
		})
	}
	if _, err := mintVoiceToken(context.Background(), nil, ""); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}
	if _, err := mintVoiceToken(ctx, client, "private-key"); err == nil || strings.Contains(err.Error(), "private-key") {
		t.Fatal(err)
	}
}

func TestVoiceAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.env")
	if got, err := voiceAPIKey("from-env", path); err != nil || got != "from-env" {
		t.Fatal(got, err)
	}
	if got, err := voiceAPIKey("", path); err != nil || got != "" {
		t.Fatal(got, err)
	}
	for _, line := range []string{"OPENAI_API_KEY=literal", "export OPENAI_API_KEY='literal'", `OPENAI_API_KEY="literal"`} {
		os.WriteFile(path, []byte("IGNORED=$(exit 99)\n#OPENAI_API_KEY=ignored\n"+line+"\n"), 0o600)
		if got, err := voiceAPIKey("", path); err != nil || got != "literal" {
			t.Fatal(got, err)
		}
	}
	for _, line := range []string{`OPENAI_API_KEY="unclosed`, "OPENAI_API_KEY=$(touch /tmp/no)", "OPENAI_API_KEY=`anything`", "OPENAI_API_KEY=two words"} {
		os.WriteFile(path, []byte(line), 0o600)
		if _, err := voiceAPIKey("", path); err == nil {
			t.Fatal("accepted nonliteral value")
		}
		if got, err := voiceAPIKey("env-wins", path); err != nil || got != "env-wins" {
			t.Fatal(got, err)
		}
	}
}

func TestCancelOnInputClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	go cancelOnInputClose(reader, cancel)
	if _, err := writer.Write([]byte("ignored input")); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("canceled while stdin was open")
	}
	writer.Close()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stdin EOF did not cancel")
	}
}

func TestCloudSpawnAuthority(t *testing.T) {
	args := []string{"spawn", "--label", "Mobile task", "pi", "/work"}
	got := cloudSpawnArgs(args, control.Requester{Human: true})
	if !reflect.DeepEqual(got, []string{"spawn", "--role", "manager", "--label", "Mobile task", "pi", "/work"}) {
		t.Fatal(got)
	}
	for _, role := range []control.Role{control.RoleWorker, control.RoleManager, control.RoleController} {
		if got := cloudSpawnArgs(args, control.Requester{Role: role}); !reflect.DeepEqual(got, args) {
			t.Fatalf("changed %s authority", role)
		}
	}
}

func TestPersona(t *testing.T) {
	dir := t.TempDir()
	if _, err := persona(dir); err == nil || !strings.Contains(err.Error(), "IDENTITY.md") {
		t.Fatalf("missing files should name the file, got %v", err)
	}
	for name, body := range map[string]string{"IDENTITY.md": "# Carla\n", "SOUL.md": "\n# Soul\n\n", "SLOP.md": "# Slop"} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
	}
	got, err := persona(dir)
	if err != nil || got != "# Carla\n\n# Soul\n\n# Slop\n" {
		t.Fatalf("persona=%q, %v", got, err)
	}
}

func TestVoiceSample(t *testing.T) {
	var got map[string]string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		json.NewDecoder(r.Body).Decode(&got)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("MP3"))}, nil
	})}
	audio, err := voiceSample(context.Background(), client, "sk-test", "marin", "Irish")
	if err != nil || string(audio) != "MP3" || got["voice"] != "marin" || !strings.HasPrefix(got["instructions"], "Speak Irish English") {
		t.Fatalf("sample=%q err=%v request=%v", audio, err, got)
	}
	if _, err := voiceSample(context.Background(), client, "sk-test", "hal", ""); err == nil {
		t.Fatal("unknown voice accepted")
	}
	if _, err := voiceSample(context.Background(), client, "sk-test", "marin", "Irish; rm -rf"); err == nil {
		t.Fatal("bad accent accepted")
	}
}
