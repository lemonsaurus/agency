package session

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/lemonsaurus/agency/internal/agents"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/layout"
	"github.com/lemonsaurus/agency/internal/status"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// paneColors is a curated palette of vibrant but soft colors. Each new pane
// gets the next color in sequence, cycling back after 12.
var paneColors = []string{
	"#89b4fa", // blue
	"#a6e3a1", // green
	"#fab387", // peach
	"#f38ba8", // red
	"#cba6f7", // mauve
	"#94e2d5", // teal
	"#f9e2af", // yellow
	"#89dceb", // sky
	"#f5c2e7", // pink
	"#b4befe", // lavender
	"#eba0ac", // maroon
	"#74c7ec", // sapphire
}

// TrackedPane holds state for a single managed pane.
type TrackedPane struct {
	PaneID    string
	AgentType string // empty for custom commands
	AgentName string // display name like "🔒 claudejail@myproject"
	Command   string
	Status    string
}

// Manager tracks panes, handles spawn/kill, and satisfies ipc.Handler.
type Manager struct {
	mu       sync.Mutex
	tmux     *tmux.Client
	registry *agents.Registry
	cfg      *config.Config
	poller   *status.Poller

	panes         map[string]*TrackedPane // keyed by pane ID
	counters      map[string]int          // instance counters per agent type (fallback when no dir)
	colorIndex    int                     // cycles through paneColors
	currentLayout string                  // last applied layout name (for relayout)
}

// NewManager creates a session manager.
func NewManager(tmuxClient *tmux.Client, registry *agents.Registry, cfg *config.Config, poller *status.Poller) *Manager {
	return &Manager{
		tmux:     tmuxClient,
		registry: registry,
		cfg:      cfg,
		poller:   poller,
		panes:    make(map[string]*TrackedPane),
		counters: make(map[string]int),
	}
}

// SpawnAgent spawns a new pane running the named agent.
func (m *Manager) SpawnAgent(ctx context.Context, name, dir string) error {
	agent, ok := m.registry.Get(name)
	if !ok {
		return fmt.Errorf("unknown agent type: %q", name)
	}
	return m.spawnPane(ctx, name, agent.Command, dir)
}

// SpawnCommand spawns a pane running an arbitrary command.
func (m *Manager) SpawnCommand(ctx context.Context, command, dir string) error {
	return m.spawnPane(ctx, "", command, dir)
}

func (m *Manager) spawnPane(ctx context.Context, agentType, command, dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	paneID, err := m.tmux.SplitWindow(ctx, command, dir)
	if err != nil {
		return fmt.Errorf("splitting window: %w", err)
	}

	// Pick the next unique color from the palette.
	color := paneColors[m.colorIndex%len(paneColors)]
	m.colorIndex++

	// Build display label: "icon agenttype@foldername" or fallback with counter.
	var displayName string
	if agentType != "" {
		agent, _ := m.registry.Get(agentType)
		folder := folderLabel(dir)
		if folder != "" {
			displayName = fmt.Sprintf("%s %s@%s", agent.Icon, agentType, folder)
		} else {
			m.counters[agentType]++
			displayName = fmt.Sprintf("%s %s #%d", agent.Icon, agentType, m.counters[agentType])
		}
	} else {
		folder := folderLabel(dir)
		if folder != "" {
			displayName = fmt.Sprintf(">_ %s@%s", filepath.Base(command), folder)
		} else {
			m.counters["terminal"]++
			displayName = fmt.Sprintf(">_ terminal #%d", m.counters["terminal"])
		}
	}

	m.stylePaneLabel(ctx, paneID, displayName, color)

	tracked := &TrackedPane{
		PaneID:    paneID,
		AgentType: agentType,
		AgentName: displayName,
		Command:   command,
		Status:    status.StatusRunning,
	}
	m.panes[paneID] = tracked

	if m.poller != nil {
		m.poller.Track(paneID, agentType)
	}

	_ = m.applyLayout(ctx, m.cfg.Session.DefaultLayout)

	return nil
}

// stylePaneLabel stores the display label and color as pane user options.
// The pane-focus-in hook in tmux.conf reads @agent_color to dynamically
// set the active border color when a pane gains focus.
func (m *Manager) stylePaneLabel(ctx context.Context, paneID, title, color string) {
	_ = m.tmux.SetPaneOption(ctx, paneID, "@agency_label", title)
	if color != "" {
		_ = m.tmux.SetPaneOption(ctx, paneID, "@agent_color", color)
	}
}

// folderLabel returns the base directory name for use in pane labels.
// Returns empty string when the dir is root, home, or unset.
func folderLabel(dir string) string {
	if dir == "" {
		return ""
	}
	base := filepath.Base(dir)
	if base == "." || base == "/" || base == "" {
		return ""
	}
	return base
}

// KillPane kills a specific pane.
func (m *Manager) KillPane(ctx context.Context, paneID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.tmux.KillPane(ctx, paneID); err != nil {
		return err
	}
	if m.poller != nil {
		m.poller.Untrack(paneID)
	}
	delete(m.panes, paneID)
	return nil
}

// KillAll kills all tracked agent panes.
func (m *Manager) KillAll(ctx context.Context) error {
	m.mu.Lock()
	ids := make([]string, 0, len(m.panes))
	for id := range m.panes {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		if err := m.KillPane(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// ListPanes returns all tracked panes.
func (m *Manager) ListPanes() []TrackedPane {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]TrackedPane, 0, len(m.panes))
	for _, p := range m.panes {
		out = append(out, *p)
	}
	return out
}

// SetLayout changes the tmux layout.
func (m *Manager) SetLayout(ctx context.Context, name string) error {
	m.mu.Lock()
	m.currentLayout = name
	m.mu.Unlock()
	return m.applyLayout(ctx, name)
}

// Relayout re-applies the current layout (useful after window resize).
func (m *Manager) Relayout(ctx context.Context) error {
	m.mu.Lock()
	name := m.currentLayout
	m.mu.Unlock()
	if name == "" {
		name = m.cfg.Session.DefaultLayout
	}
	return m.applyLayout(ctx, name)
}

func (m *Manager) applyLayout(ctx context.Context, name string) error {
	if name == "tiled" {
		return m.applyCustomTiled(ctx)
	}
	return m.tmux.SelectLayout(ctx, tmuxLayout(name))
}

func (m *Manager) applyCustomTiled(ctx context.Context) error {
	info, err := m.tmux.GetWindowInfo(ctx)
	if err != nil {
		// Fallback to tmux's built-in tiled.
		return m.tmux.SelectLayout(ctx, "tiled")
	}
	if info.PaneCount <= 1 {
		return nil
	}

	maxRows := m.cfg.Session.MaxRows
	if maxRows <= 0 {
		maxRows = 3
	}
	// Prefer fewer rows when pane count is small (keeps panes taller).
	// Only use the full maxRows once we exceed what 2 rows can handle
	// in 3 columns (i.e. more than 6 panes).
	effectiveMax := min(2, maxRows)
	if info.PaneCount > effectiveMax*3 {
		effectiveMax = maxRows
	}

	columns := layout.Grid(info.PaneCount, effectiveMax)
	layoutStr := layout.BuildCustomLayout(info.Width, info.Height, columns)
	return m.tmux.SelectLayout(ctx, layoutStr)
}

// AdoptOrphans scans existing tmux panes and rebuilds internal state.
func (m *Manager) AdoptOrphans(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return fmt.Errorf("listing panes for adoption: %w", err)
	}

	for _, pane := range panes {
		if _, exists := m.panes[pane.ID]; exists {
			continue
		}

		color := paneColors[m.colorIndex%len(paneColors)]
		m.colorIndex++

		agentType := m.registry.DetectType(pane.Command)
		var displayName string
		if agentType != "" {
			agent, _ := m.registry.Get(agentType)
			folder := folderLabel(pane.CWD)
			if folder != "" {
				displayName = fmt.Sprintf("%s %s@%s", agent.Icon, agentType, folder)
			} else {
				m.counters[agentType]++
				displayName = fmt.Sprintf("%s %s #%d", agent.Icon, agentType, m.counters[agentType])
			}
		} else {
			folder := folderLabel(pane.CWD)
			if folder != "" {
				displayName = fmt.Sprintf(">_ %s@%s", pane.Command, folder)
			} else {
				m.counters["terminal"]++
				displayName = fmt.Sprintf(">_ terminal #%d", m.counters["terminal"])
			}
		}

		m.stylePaneLabel(ctx, pane.ID, displayName, color)

		tracked := &TrackedPane{
			PaneID:    pane.ID,
			AgentType: agentType,
			AgentName: displayName,
			Command:   pane.Command,
			Status:    status.StatusIdle,
		}
		m.panes[pane.ID] = tracked

		if m.poller != nil {
			m.poller.Track(pane.ID, agentType)
		}
	}

	return nil
}

// BroadcastKeys sends keystrokes to all tracked agent panes (skips
// plain terminal/custom command panes that have no agent type).
func (m *Manager) BroadcastKeys(ctx context.Context, keys string) error {
	m.mu.Lock()
	ids := make([]string, 0, len(m.panes))
	for id, p := range m.panes {
		if p.AgentType != "" {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()

	for _, id := range ids {
		if err := m.tmux.SendText(ctx, id, keys); err != nil {
			return fmt.Errorf("sending keys to pane %s: %w", id, err)
		}
	}
	return nil
}

// PaneCount returns the number of tracked panes.
func (m *Manager) PaneCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.panes)
}

// tmuxLayout maps our layout names to tmux layout names.
func tmuxLayout(layout string) string {
	switch layout {
	case "tiled":
		return "tiled"
	case "columns":
		return "even-horizontal"
	case "rows":
		return "even-vertical"
	case "main-vertical":
		return "main-vertical"
	default:
		return "tiled"
	}
}
