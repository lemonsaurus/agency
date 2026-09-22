package ipc

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemonsaurus/agency/internal/control"
)

func TestSpawnTaskLabelPayload(t *testing.T) {
	for _, target := range []string{`"agent":"pi"`, `"command":"pi --no-session"`} {
		for _, window := range []string{"", "project"} {
			for _, label := range []string{"", "   ", "bad\nlabel", "bad\u2028label", strings.Repeat("界", 101), "Cloud Harness Setup"} {
				h := &mockHandler{requester: control.Requester{PaneID: "%0", Role: control.RoleController}}
				srv := NewServer("", h)
				encoded, _ := json.Marshal(label)
				cmd := "spawn-role:"
				if window != "" {
					cmd = "spawn-window:"
				}
				message := cmd + `{` + target + `,"role":"manager","window":"` + window + `","label":` + string(encoded) + `}`
				_, err := srv.dispatch(message, 123)
				valid := label == "Cloud Harness Setup"
				if (err == nil) != valid {
					t.Fatalf("dispatch(%s) = %v", message, err)
				}
				if !valid {
					if len(h.spawns)+len(h.commands) != 0 {
						t.Fatal("invalid label reached spawn handler")
					}
					continue
				}
				if len(h.spawns) == 1 {
					if got := h.spawns[0]; got.label != label || got.window != window {
						t.Fatalf("spawn = %+v", got)
					}
				} else if len(h.commands) == 1 {
					if got := h.commands[0]; got.label != label || got.window != window {
						t.Fatalf("command = %+v", got)
					}
				} else {
					t.Fatal("spawn handler was not called")
				}
			}
		}
	}
}

func TestLabelProtocolPreservesEmptyAndErrorLikeLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agency.sock")
	h := &mockHandler{requester: control.Requester{PaneID: "%7", Role: control.RoleWorker}}
	srv := NewServer(path, h)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	for _, label := range []string{"", "error: not an error", `Quotes " and #{pane_id} 雲`} {
		encoded, _ := json.Marshal(label)
		if _, err := SendMessage(path, `label:{"label":`+string(encoded)+`}`); err != nil {
			t.Fatal(err)
		}
		response, err := SendMessage(path, `label:{}`)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		if err := json.Unmarshal([]byte(response), &got); err != nil || got != label {
			t.Fatalf("response = %q, label = %q, error = %v", response, got, err)
		}
	}
}

func TestLabelIfEmptyProtocol(t *testing.T) {
	h := &mockHandler{requester: control.Requester{PaneID: "%7", Role: control.RoleWorker}}
	srv := NewServer("", h)
	for _, label := range []string{"Cloud Setup", "Replacement"} {
		encoded, _ := json.Marshal(label)
		got, err := srv.dispatch(`label-if-empty:{"label":`+string(encoded)+`}`, 123)
		if err != nil || got != `"Cloud Setup"` {
			t.Fatalf("initialize = %q, %v", got, err)
		}
	}
	if h.labelRequester.PaneID != "%7" {
		t.Fatalf("requester = %+v", h.labelRequester)
	}
	for _, message := range []string{`label-if-empty:{}`, `label-if-empty:{"pane":"%9","label":"Other"}`} {
		if _, err := srv.dispatch(message, 123); err == nil {
			t.Fatalf("accepted invalid initializer: %s", message)
		}
	}
	if _, err := srv.dispatch(`label-if-empty:{"label":"Task"}`, 0); err == nil {
		t.Fatal("accepted an unauthenticated requester")
	}
}

func TestLabelUsesAuthenticatedRequester(t *testing.T) {
	h := &mockHandler{requester: control.Requester{PaneID: "%7", Role: control.RoleWorker}}
	srv := NewServer("", h)
	if _, err := srv.dispatch(`label:{"pane":"%9","label":"Task"}`, 123); err != nil {
		t.Fatal(err)
	}
	if h.labelRequester.PaneID != "%7" || h.labelRequester.Role != control.RoleWorker || h.labelPane != "%9" {
		t.Fatalf("requester=%+v, target=%q", h.labelRequester, h.labelPane)
	}
	if _, err := srv.dispatch(`label:{}`, 0); err == nil {
		t.Fatal("accepted an unauthenticated requester")
	}
}
