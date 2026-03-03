package config

import (
	"fmt"
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml/v2"
)

type Config struct {
	Session SessionConfig            `toml:"session"`
	Theme   ThemeConfig              `toml:"theme"`
	Agents  map[string]AgentConfig   `toml:"agents"`
	// AgentOrder preserves insertion order from TOML parsing.
	AgentOrder []string `toml:"-"`
}

type SessionConfig struct {
	Name          string `toml:"name"`
	DefaultLayout string `toml:"default_layout"`
}

type ThemeConfig struct {
	ActiveBorder   string `toml:"active_border"`
	InactiveBorder string `toml:"inactive_border"`
	StatusBG       string `toml:"status_bg"`
	StatusFG       string `toml:"status_fg"`
}

type AgentConfig struct {
	Command     string `toml:"command"`
	Icon        string `toml:"icon"`
	BorderColor string `toml:"border_color"`
}

var validLayouts = map[string]bool{
	"tiled":         true,
	"columns":       true,
	"rows":          true,
	"main-vertical": true,
}

func Load() (*Config, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return DefaultConfig(), nil
	}
	path := filepath.Join(configDir, "agency", "config.toml")
	return LoadFromPath(path)
}

func LoadFromPath(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	cfg := DefaultConfig()

	// Parse into a raw map first to extract agent ordering.
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Extract agent order from the raw map.
	if agentsRaw, ok := raw["agents"]; ok {
		if agentsMap, ok := agentsRaw.(map[string]any); ok {
			cfg.AgentOrder = make([]string, 0, len(agentsMap))
			for name := range agentsMap {
				cfg.AgentOrder = append(cfg.AgentOrder, name)
			}
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.Session.DefaultLayout != "" && !validLayouts[c.Session.DefaultLayout] {
		return fmt.Errorf("invalid layout %q: must be one of tiled, columns, rows, main-vertical", c.Session.DefaultLayout)
	}
	for name, agent := range c.Agents {
		if agent.Command == "" {
			return fmt.Errorf("agent %q: command is required", name)
		}
	}
	return nil
}
