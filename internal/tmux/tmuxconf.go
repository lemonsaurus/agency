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

	// Prefix.
	b.WriteString("# Prefix\n")
	fmt.Fprintf(&b, "unbind C-b\n")
	fmt.Fprintf(&b, "set -g prefix %s\n", cfg.Keys.Prefix)
	fmt.Fprintf(&b, "bind %s send-prefix\n\n", cfg.Keys.Prefix)

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
	b.WriteString("set -ga terminal-overrides \",*256col*:Tc\"\n")
	// Extended keys: required for modern terminals (Rio, Ghostty, WezTerm, etc.)
	// that use Kitty keyboard protocol / CSI u to properly pass Ctrl+Space
	// and other modified keys to tmux.
	b.WriteString("set -g extended-keys always\n")
	b.WriteString("set -gs extended-keys-format csi-u\n")
	b.WriteString("set -as terminal-features 'xterm*:extkeys'\n")
	b.WriteString("set -as terminal-features 'tmux*:extkeys'\n\n")

	// Catppuccin Mocha colors.
	b.WriteString("# Catppuccin Mocha theme\n")
	fmt.Fprintf(&b, "set -g pane-border-style fg=%s\n", cfg.Theme.InactiveBorder)
	// Active border: dynamically resolves to the focused pane's @agent_color.
	fmt.Fprintf(&b, "set -g pane-active-border-style \"#{?#{@agent_color},fg=#{@agent_color},fg=%s}\"\n", cfg.Theme.ActiveBorder)
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
	fmt.Fprintf(&b, "bind %s display-popup -E -w 40 -h 15 \"%s palette\"\n", cfg.Keys.Palette, agencyBin)

	// Terminal: spawn a tracked terminal pane via agency (so it gets a label + color).
	fmt.Fprintf(&b, "bind %s run-shell \"%s spawn --cmd \\\"$SHELL\\\" #{pane_current_path}\"\n", cfg.Keys.Terminal, agencyBin)

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
	fmt.Fprintf(&b, "bind %s run-shell \"%s layout tiled\"\n", cfg.Keys.LayoutTiled, agencyBin)
	fmt.Fprintf(&b, "bind %s run-shell \"%s layout columns\"\n", cfg.Keys.LayoutColumns, agencyBin)
	fmt.Fprintf(&b, "bind %s run-shell \"%s layout rows\"\n", cfg.Keys.LayoutRows, agencyBin)
	fmt.Fprintf(&b, "bind %s run-shell \"%s layout main-vertical\"\n", cfg.Keys.LayoutMainVert, agencyBin)
	fmt.Fprintf(&b, "bind %s next-layout\n\n", cfg.Keys.LayoutCycle)

	// Management.
	b.WriteString("# Management\n")
	fmt.Fprintf(&b, "bind %s confirm-before -p \"Kill pane? (y/n)\" kill-pane\n", cfg.Keys.KillPane)
	// Kill session: popup accepts Enter or y/Y as confirmation.
	killCmd := `printf 'Kill session? [Enter/y]: '; read -r _k; case "$_k" in ""|y|Y) tmux kill-session;; esac`
	escapedKill := strings.ReplaceAll(killCmd, `"`, `\"`)
	fmt.Fprintf(&b, "bind %s display-popup -E -w 44 -h 3 \"%s\"\n", cfg.Keys.KillSession, escapedKill)
	fmt.Fprintf(&b, "bind %s resize-pane -Z\n", cfg.Keys.Zoom)
	fmt.Fprintf(&b, "bind %s set-window-option synchronize-panes\n", cfg.Keys.Broadcast)
	fmt.Fprintf(&b, "bind %s detach-client\n", cfg.Keys.Detach)
	fmt.Fprintf(&b, "bind %s respawn-pane -k\n", cfg.Keys.Respawn)
	fmt.Fprintf(&b, "bind %s copy-mode\n", cfg.Keys.CopyMode)
	fmt.Fprintf(&b, "bind %s paste-buffer\n", cfg.Keys.Paste)

	return b.String()
}
