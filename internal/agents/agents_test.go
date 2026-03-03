package agents

import (
	"testing"

	"github.com/lemonsaurus/agency/internal/config"
)

func testAgents() map[string]config.AgentConfig {
	return map[string]config.AgentConfig{
		"claude": {Command: "claude", Icon: "🤖", BorderColor: "#cba6f7"},
		"codex":  {Command: "codex", Icon: "🧠", BorderColor: "#89b4fa"},
		"gemini": {Command: "gemini", Icon: "✦", BorderColor: "#f9e2af"},
		"aider":  {Command: "aider --model ollama_chat/gemma3", Icon: "🔧", BorderColor: "#a6e3a1"},
	}
}

func TestRegistryGet(t *testing.T) {
	r := NewRegistry(testAgents(), nil)

	agent, ok := r.Get("claude")
	if !ok {
		t.Fatal("expected to find 'claude'")
	}
	if agent.Command != "claude" {
		t.Errorf("expected command 'claude', got %q", agent.Command)
	}

	_, ok = r.Get("nonexistent")
	if ok {
		t.Error("expected 'nonexistent' to not be found")
	}
}

func TestRegistryNames(t *testing.T) {
	r := NewRegistry(testAgents(), []string{"claude", "codex", "gemini", "aider"})
	names := r.Names()
	if len(names) != 4 {
		t.Fatalf("expected 4 names, got %d", len(names))
	}
	if names[0] != "claude" {
		t.Errorf("expected first name 'claude', got %q", names[0])
	}
}

func TestRegistryNamesAlphabeticalFallback(t *testing.T) {
	r := NewRegistry(testAgents(), nil)
	names := r.Names()
	// Should be alphabetical when no order provided.
	if names[0] != "aider" {
		t.Errorf("expected first name 'aider' (alphabetical), got %q", names[0])
	}
}

func TestDetectType(t *testing.T) {
	r := NewRegistry(testAgents(), nil)

	tests := []struct {
		cmdline  string
		expected string
	}{
		{"claude", "claude"},
		{"/usr/bin/claude --flag", "claude"},
		{"codex", "codex"},
		{"gemini", "gemini"},
		{"aider --model ollama_chat/gemma3", "aider"},
		{"/home/user/.local/bin/aider --yes", "aider"},
		{"unknown-cmd", ""},
		{"", ""},
		{"bash", ""},
	}

	for _, tt := range tests {
		got := r.DetectType(tt.cmdline)
		if got != tt.expected {
			t.Errorf("DetectType(%q) = %q, want %q", tt.cmdline, got, tt.expected)
		}
	}
}

func TestNamesReturnsCopy(t *testing.T) {
	r := NewRegistry(testAgents(), []string{"claude", "codex"})
	names := r.Names()
	names[0] = "modified"
	original := r.Names()
	if original[0] == "modified" {
		t.Error("Names() should return a copy, not the internal slice")
	}
}
