package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Session.Name != "agency" {
		t.Errorf("expected session name 'agency', got %q", cfg.Session.Name)
	}
	if cfg.Session.DefaultLayout != "tiled" {
		t.Errorf("expected default layout 'tiled', got %q", cfg.Session.DefaultLayout)
	}
	if len(cfg.Agents) != 4 {
		t.Errorf("expected 4 default agents, got %d", len(cfg.Agents))
	}
	if cfg.Agents["claude"].Command != "claude" {
		t.Errorf("expected claude command 'claude', got %q", cfg.Agents["claude"].Command)
	}
	if cfg.Agents["claude"].Icon != "🤖" {
		t.Errorf("expected claude icon '🤖', got %q", cfg.Agents["claude"].Icon)
	}
	if cfg.Keys.Prefix != "C-b" {
		t.Errorf("expected default prefix 'C-b', got %q", cfg.Keys.Prefix)
	}
	if cfg.Keys.Zoom != "z" {
		t.Errorf("expected default zoom 'z', got %q", cfg.Keys.Zoom)
	}
}

func TestParse(t *testing.T) {
	tomlData := `
[session]
name = "test-session"
default_layout = "columns"

[theme]
active_border = "#ff0000"

[agents.myagent]
command = "my-agent-cmd"
icon = "X"
border_color = "#00ff00"
`
	cfg, err := Parse([]byte(tomlData))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Session.Name != "test-session" {
		t.Errorf("expected session name 'test-session', got %q", cfg.Session.Name)
	}
	if cfg.Session.DefaultLayout != "columns" {
		t.Errorf("expected layout 'columns', got %q", cfg.Session.DefaultLayout)
	}
	if cfg.Theme.ActiveBorder != "#ff0000" {
		t.Errorf("expected active border '#ff0000', got %q", cfg.Theme.ActiveBorder)
	}
	agent, ok := cfg.Agents["myagent"]
	if !ok {
		t.Fatal("expected agent 'myagent' in config")
	}
	if agent.Command != "my-agent-cmd" {
		t.Errorf("expected command 'my-agent-cmd', got %q", agent.Command)
	}
}

func TestParseKeys(t *testing.T) {
	tomlData := `
[keys]
prefix = "C-Space"
palette = "p"
zoom = "f"
copy_mode = "c"
paste = "v"

[agents.test]
command = "test"
icon = "T"
border_color = "#000"
`
	cfg, err := Parse([]byte(tomlData))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Keys.Prefix != "C-Space" {
		t.Errorf("expected prefix 'C-Space', got %q", cfg.Keys.Prefix)
	}
	if cfg.Keys.Palette != "p" {
		t.Errorf("expected palette 'p', got %q", cfg.Keys.Palette)
	}
	if cfg.Keys.Zoom != "f" {
		t.Errorf("expected zoom 'f', got %q", cfg.Keys.Zoom)
	}
	if cfg.Keys.CopyMode != "c" {
		t.Errorf("expected copy_mode 'c', got %q", cfg.Keys.CopyMode)
	}
	if cfg.Keys.Paste != "v" {
		t.Errorf("expected paste 'v', got %q", cfg.Keys.Paste)
	}
	// Unset keys should retain defaults.
	if cfg.Keys.KillPane != "x" {
		t.Errorf("expected default kill_pane 'x', got %q", cfg.Keys.KillPane)
	}
	if cfg.Keys.Detach != "d" {
		t.Errorf("expected default detach 'd', got %q", cfg.Keys.Detach)
	}
}

func TestParseInvalidLayout(t *testing.T) {
	tomlData := `
[session]
default_layout = "bogus"
`
	_, err := Parse([]byte(tomlData))
	if err == nil {
		t.Fatal("expected error for invalid layout, got nil")
	}
}

func TestParseMissingCommand(t *testing.T) {
	tomlData := `
[agents.broken]
icon = "X"
`
	_, err := Parse([]byte(tomlData))
	if err == nil {
		t.Fatal("expected error for missing command, got nil")
	}
}

func TestValidateLayouts(t *testing.T) {
	for _, layout := range []string{"tiled", "columns", "rows", "main-vertical"} {
		cfg := DefaultConfig()
		cfg.Session.DefaultLayout = layout
		if err := cfg.Validate(); err != nil {
			t.Errorf("layout %q should be valid, got error: %v", layout, err)
		}
	}
}

func TestLoadFromMissingPath(t *testing.T) {
	cfg, err := LoadFromPath("/nonexistent/path/config.toml")
	if err != nil {
		t.Fatalf("expected fallback to defaults, got error: %v", err)
	}
	if cfg.Session.Name != "agency" {
		t.Errorf("expected default config, got session name %q", cfg.Session.Name)
	}
}

func TestLoadFromPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	data := `
[session]
name = "from-file"

[agents.test]
command = "test-cmd"
icon = "T"
border_color = "#000"
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath failed: %v", err)
	}
	if cfg.Session.Name != "from-file" {
		t.Errorf("expected 'from-file', got %q", cfg.Session.Name)
	}
}
