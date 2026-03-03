package palette

import (
	"testing"
)

func TestDialogModelInit(t *testing.T) {
	m := newDialogModel("claude", "agency", "/home/user")

	if m.agentName != "claude" {
		t.Errorf("expected agentName 'claude', got %q", m.agentName)
	}
	if m.agencyBin != "agency" {
		t.Errorf("expected agencyBin 'agency', got %q", m.agencyBin)
	}
	if m.textInput.Value() != "/home/user" {
		t.Errorf("expected default dir '/home/user', got %q", m.textInput.Value())
	}
	if m.submitted {
		t.Error("expected submitted to be false initially")
	}
	if m.quitting {
		t.Error("expected quitting to be false initially")
	}
}

func TestDialogModelInitEmptyDir(t *testing.T) {
	m := newDialogModel("codex", "agency", "")

	if m.textInput.Value() != "" {
		t.Errorf("expected empty default dir, got %q", m.textInput.Value())
	}
}

func TestDialogModelDirNotSubmitted(t *testing.T) {
	m := newDialogModel("claude", "agency", "/tmp")
	// Not submitted, so Dir() should return empty.
	if m.Dir() != "" {
		t.Errorf("expected empty dir when not submitted, got %q", m.Dir())
	}
}

func TestDialogModelDirSubmitted(t *testing.T) {
	m := newDialogModel("claude", "agency", "/tmp/project")
	m.submitted = true
	if m.Dir() != "/tmp/project" {
		t.Errorf("expected '/tmp/project', got %q", m.Dir())
	}
}

func TestDialogModelView(t *testing.T) {
	m := newDialogModel("claude", "agency", "/tmp")
	view := m.View()

	if view == "" {
		t.Error("expected non-empty view")
	}

	// Should contain agent name.
	if !containsStr(view, "claude") {
		t.Error("expected view to contain 'claude'")
	}
	// Should contain hint text.
	if !containsStr(view, "Enter confirm") {
		t.Error("expected view to contain 'Enter confirm'")
	}
}

func TestDialogModelViewQuitting(t *testing.T) {
	m := newDialogModel("claude", "agency", "/tmp")
	m.quitting = true
	if m.View() != "" {
		t.Error("expected empty view when quitting")
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && searchStr(s, sub)
}

func searchStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
