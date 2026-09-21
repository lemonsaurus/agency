package main

import (
	"context"
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
