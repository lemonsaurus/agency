package config

func DefaultConfig() *Config {
	return &Config{
		Session: SessionConfig{
			Name:          "agency",
			DefaultLayout: "tiled",
		},
		Theme: ThemeConfig{
			ActiveBorder:   "#89b4fa",
			InactiveBorder: "#45475a",
			StatusBG:       "#181825",
			StatusFG:       "#cdd6f4",
		},
		Keys: KeysConfig{
			Prefix:         "C-b",
			Palette:        "a",
			Terminal:       "1",
			LayoutTiled:    "=",
			LayoutColumns:  "|",
			LayoutRows:     "-",
			LayoutMainVert: "m",
			LayoutCycle:    "Space",
			KillPane:       "x",
			KillSession:    "q",
			Zoom:           "z",
			Broadcast:      "b",
			Detach:         "d",
			Respawn:        "r",
			CopyMode:       "[",
			Paste:          "]",
		},
		Agents: map[string]AgentConfig{
			"claudejail": {Command: "claudejail", Icon: "🔒", BorderColor: "#f38ba8"},
			"claude":     {Command: "claude", Icon: "🤖", BorderColor: "#cba6f7"},
			"codex":      {Command: "codex", Icon: "🧠", BorderColor: "#89b4fa"},
			"gemini":     {Command: "gemini", Icon: "✦", BorderColor: "#f9e2af"},
		},
		AgentOrder: []string{"claudejail", "claude", "codex", "gemini"},
	}
}
