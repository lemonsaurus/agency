package status

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestDetectClaude(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{"empty", "", StatusIdle},
		{"waiting prompt", "some output\n❯ ", StatusWaiting},
		{"tool use read", "some output\nRead(/home/user/file.go)", StatusRunning},
		{"tool use bash", "some output\nBash(ls -la)", StatusRunning},
		{"tool use edit", "some output\nEdit(/home/user/file.go)", StatusRunning},
		{"no indicators", "just some text output", StatusIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect("claude", tt.content)
			if got != tt.expected {
				t.Errorf("Detect(claude, %q) = %q, want %q", tt.content, got, tt.expected)
			}
		})
	}
}

func TestDetectCodex(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{"waiting", "output\n> ", StatusWaiting},
		{"thinking", "output\nThinking...", StatusRunning},
		{"running", "output\nRunning command", StatusRunning},
		{"idle", "some output", StatusIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect("codex", tt.content)
			if got != tt.expected {
				t.Errorf("Detect(codex, %q) = %q, want %q", tt.content, got, tt.expected)
			}
		})
	}
}

func TestDetectGemini(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{"waiting", ">>> ", StatusWaiting},
		{"generating", "Generating response...", StatusRunning},
		{"idle", "done", StatusIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect("gemini", tt.content)
			if got != tt.expected {
				t.Errorf("Detect(gemini, %q) = %q, want %q", tt.content, got, tt.expected)
			}
		})
	}
}

func TestDetectGeneric(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{"shell prompt", "user@host:~$ ", StatusWaiting},
		{"no prompt", "running something", StatusIdle},
		{"empty", "", StatusIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect("unknown", tt.content)
			if got != tt.expected {
				t.Errorf("Detect(unknown, %q) = %q, want %q", tt.content, got, tt.expected)
			}
		})
	}
}

func TestLastN(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}
	got := lastN(lines, 3)
	if len(got) != 3 || got[0] != "c" {
		t.Errorf("lastN(5, 3) = %v, want [c d e]", got)
	}

	got = lastN(lines, 10)
	if len(got) != 5 {
		t.Errorf("lastN(5, 10) should return all 5, got %d", len(got))
	}
}

// mockCapture implements PaneCapture for testing.
type mockCapture struct {
	mu      sync.Mutex
	content map[string]string
}

func (m *mockCapture) CapturePaneContent(_ context.Context, paneID string, _ int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.content[paneID], nil
}

func TestPollerTrackUntrack(t *testing.T) {
	capture := &mockCapture{content: map[string]string{}}
	p := NewPoller(capture, nil)

	p.Track("%0", "claude")
	p.Track("%1", "codex")

	p.mu.Lock()
	if len(p.panes) != 2 {
		t.Errorf("expected 2 tracked panes, got %d", len(p.panes))
	}
	p.mu.Unlock()

	p.Untrack("%0")
	p.mu.Lock()
	if len(p.panes) != 1 {
		t.Errorf("expected 1 tracked pane after untrack, got %d", len(p.panes))
	}
	p.mu.Unlock()
}

func TestPollerCallsCallback(t *testing.T) {
	capture := &mockCapture{content: map[string]string{
		"%0": "❯ ",
	}}

	var callbackMu sync.Mutex
	var callbacks []string
	cb := func(paneID, agentType, status string) {
		callbackMu.Lock()
		callbacks = append(callbacks, paneID+":"+status)
		callbackMu.Unlock()
	}

	p := NewPoller(capture, cb)
	p.interval = 50 * time.Millisecond
	p.Track("%0", "claude")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	p.Run(ctx)

	callbackMu.Lock()
	defer callbackMu.Unlock()
	if len(callbacks) == 0 {
		t.Error("expected at least one callback")
	}
	if len(callbacks) > 0 && callbacks[0] != "%0:waiting" {
		t.Errorf("expected callback '%s:waiting', got %q", "%0", callbacks[0])
	}
}
