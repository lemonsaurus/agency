package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lemonsaurus/agency/internal/config"
)

// GenerateConfig writes an agency-specific tmux.conf and returns its path.
func GenerateConfig(cfg *config.Config, agencyBin string) (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding config dir: %w", err)
	}
	dir := filepath.Join(configDir, "agency")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating config dir: %w", err)
	}
	path := filepath.Join(dir, "tmux.conf")
	content := buildTmuxConf(cfg, agencyBin)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("writing tmux.conf: %w", err)
	}
	return path, nil
}

func buildTmuxConf(cfg *config.Config, agencyBin string) string {
	var b strings.Builder

	// Header.
	b.WriteString("# Agency tmux.conf — auto-generated, do not edit\n\n")

	// Prefix: Ctrl+Space.
	b.WriteString("# Prefix\n")
	b.WriteString("unbind C-b\n")
	b.WriteString("set -g prefix C-Space\n")
	b.WriteString("bind C-Space send-prefix\n\n")

	// General settings.
	b.WriteString("# General\n")
	b.WriteString("set -g mouse on\n")
	b.WriteString("set -g history-limit 50000\n")
	b.WriteString("set -g mode-keys emacs\n")
	b.WriteString("set -g status-keys emacs\n")
	b.WriteString("set -g base-index 1\n")
	b.WriteString("set -g pane-base-index 1\n")
	b.WriteString("set -g renumber-windows on\n")
	b.WriteString("set -g default-terminal \"tmux-256color\"\n")
	b.WriteString("set -ga terminal-overrides \",*256col*:Tc\"\n\n")

	// Catppuccin Mocha colors.
	b.WriteString("# Catppuccin Mocha theme\n")
	// Global defaults for panes that have no per-pane style (e.g. the initial shell pane).
	// Agency-spawned panes override these via set-option -p at spawn time.
	b.WriteString("set -g pane-border-style fg=#313244\n")
	b.WriteString("set -g pane-active-border-style fg=#45475a\n")
	b.WriteString("set -g pane-border-status top\n")
	b.WriteString("set -g pane-border-lines single\n")
	// Label format: show @agency_label with colored badge for agency panes,
	// fall back to live command@folder for plain terminal panes.
	// Note: #, is tmux's escape for a literal comma inside format strings.
	b.WriteString("set -g pane-border-format \"#{?#{@agency_label},#[bg=#{@agent_color}#,fg=#1e1e2e#,bold] #{@agency_label} #[default] ,#[fg=#585b70] #{pane_current_command}@#{b:pane_current_path} }\"\n\n")

	// Status bar.
	b.WriteString("# Status bar\n")
	b.WriteString("set -g status on\n")
	b.WriteString("set -g status-position bottom\n")
	b.WriteString("set -g status-interval 5\n")
	fmt.Fprintf(&b, "set -g status-style bg=%s,fg=%s\n", cfg.Theme.StatusBG, cfg.Theme.StatusFG)
	fmt.Fprintf(&b, "set -g status-left \"#[bg=#89b4fa,fg=#1e1e2e,bold] %s #[default] \"\n", cfg.Session.Name)
	b.WriteString("set -g status-left-length 30\n")
	b.WriteString("set -g status-right \"#{pane_count} panes | %H:%M \"\n")
	b.WriteString("set -g status-right-length 50\n\n")

	// Window/pane styling.
	b.WriteString("# Window styling\n")
	fmt.Fprintf(&b, "set -g window-status-style fg=%s\n", cfg.Theme.StatusFG)
	b.WriteString("set -g window-status-current-style fg=#89b4fa,bold\n")
	fmt.Fprintf(&b, "set -g message-style bg=%s,fg=%s\n\n", cfg.Theme.StatusBG, cfg.Theme.StatusFG)

	// Spawn keybindings.
	b.WriteString("# Agent spawn keybindings\n")
	fmt.Fprintf(&b, "bind a display-popup -E -w 40 -h 15 \"%s palette\"\n", agencyBin)

	// Key 1: plain terminal in current pane's directory (no dialog, no tracking).
	b.WriteString("bind 1 split-window -c \"#{pane_current_path}\"\n")

	// Keys 2-5: agent spawn dialogs pre-filled with focused pane's directory.
	i := 2
	for _, name := range cfg.AgentOrder {
		if _, ok := cfg.Agents[name]; ok {
			fmt.Fprintf(&b, "bind %d display-popup -E -w 50 -h 7 \"%s spawn-dialog %s #{pane_current_path}\"\n", i, agencyBin, name)
			i++
			if i > 5 {
				break
			}
		}
	}
	b.WriteString("\n")

	// Navigation.
	b.WriteString("# Navigation\n")
	b.WriteString("bind Up select-pane -U\n")
	b.WriteString("bind Down select-pane -D\n")
	b.WriteString("bind Left select-pane -L\n")
	b.WriteString("bind Right select-pane -R\n")
	b.WriteString("bind S-Up resize-pane -U 5\n")
	b.WriteString("bind S-Down resize-pane -D 5\n")
	b.WriteString("bind S-Left resize-pane -L 5\n")
	b.WriteString("bind S-Right resize-pane -R 5\n\n")

	// Layout keybindings.
	b.WriteString("# Layout\n")
	fmt.Fprintf(&b, "bind = run-shell \"%s layout tiled\"\n", agencyBin)
	fmt.Fprintf(&b, "bind | run-shell \"%s layout columns\"\n", agencyBin)
	fmt.Fprintf(&b, "bind - run-shell \"%s layout rows\"\n", agencyBin)
	fmt.Fprintf(&b, "bind m run-shell \"%s layout main-vertical\"\n", agencyBin)
	b.WriteString("bind Space next-layout\n\n")

	// Management.
	b.WriteString("# Management\n")
	b.WriteString("bind x confirm-before -p \"Kill pane? (y/n)\" kill-pane\n")
	// Kill session: popup accepts Enter or y/Y as confirmation.
	killCmd := `printf 'Kill session? [Enter/y]: '; read -r _k; case "$_k" in ""|y|Y) tmux kill-session;; esac`
	escapedKill := strings.ReplaceAll(killCmd, `"`, `\"`)
	fmt.Fprintf(&b, "bind q display-popup -E -w 44 -h 3 \"%s\"\n", escapedKill)
	b.WriteString("bind f resize-pane -Z\n")
	b.WriteString("bind b set-window-option synchronize-panes\n")
	b.WriteString("bind d detach-client\n")
	b.WriteString("bind r respawn-pane -k\n")
	b.WriteString("bind c copy-mode\n")
	b.WriteString("bind v paste-buffer\n")

	return b.String()
}
