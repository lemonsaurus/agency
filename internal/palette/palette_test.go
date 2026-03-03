package palette

import (
	"testing"

	"github.com/lemonsaurus/agency/internal/agents"
	"github.com/lemonsaurus/agency/internal/config"
)

func testRegistry() *agents.Registry {
	agentMap := map[string]config.AgentConfig{
		"claude": {Command: "claude", Icon: "🤖", BorderColor: "#cba6f7"},
		"codex":  {Command: "codex", Icon: "🧠", BorderColor: "#89b4fa"},
		"gemini": {Command: "gemini", Icon: "✦", BorderColor: "#f9e2af"},
	}
	return agents.NewRegistry(agentMap, []string{"claude", "codex", "gemini"})
}

func TestBuildItems(t *testing.T) {
	reg := testRegistry()
	items := buildItems(reg)

	// 3 agents + 1 custom = 4 items.
	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}

	// First item should be claude.
	first := items[0].(agentItem)
	if first.name != "claude" {
		t.Errorf("expected first item 'claude', got %q", first.name)
	}
	if first.icon != "🤖" {
		t.Errorf("expected icon '🤖', got %q", first.icon)
	}

	// Last item should be custom.
	last := items[3].(agentItem)
	if !last.custom {
		t.Error("expected last item to be custom")
	}
}

func TestAgentItemTitle(t *testing.T) {
	item := agentItem{name: "claude", icon: "🤖", command: "claude"}
	title := item.Title()
	if title != "🤖  claude" {
		t.Errorf("expected '🤖  claude', got %q", title)
	}
}

func TestAgentItemCustomTitle(t *testing.T) {
	item := agentItem{custom: true}
	title := item.Title()
	if title != "⚡ Custom command..." {
		t.Errorf("expected '⚡ Custom command...', got %q", title)
	}
}

func TestAgentItemFilterValue(t *testing.T) {
	item := agentItem{name: "claude"}
	if item.FilterValue() != "claude" {
		t.Errorf("expected FilterValue 'claude', got %q", item.FilterValue())
	}
}

func TestAgentItemDescription(t *testing.T) {
	item := agentItem{name: "claude", command: "claude --flag"}
	if item.Description() != "claude --flag" {
		t.Errorf("expected description 'claude --flag', got %q", item.Description())
	}

	custom := agentItem{custom: true}
	if custom.Description() != "Run any command" {
		t.Errorf("expected 'Run any command', got %q", custom.Description())
	}
}
