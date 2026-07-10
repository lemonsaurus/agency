package session

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"github.com/lemonsaurus/agency/internal/agents"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/control"
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
	PaneID     string
	WindowName string
	AgentType  string // empty for custom commands
	AgentName  string // display name like "🔒 claudejail@myproject"
	Command    string
	Status     string
	Role       control.Role
	ParentID   string
	RootID     string
}

// Manager tracks panes, handles spawn/kill, and satisfies ipc.Handler.
type Manager struct {
	mu       sync.Mutex
	tmux     *tmux.Client
	registry *agents.Registry
	cfg      *config.Config
	poller   *status.Poller

	panes          map[string]*TrackedPane // keyed by pane ID
	counters       map[string]int          // instance counters per agent type (fallback when no dir)
	colorIndex     int                     // cycles through paneColors
	currentLayout  string                  // last applied layout name (for relayout)
	processOwnedBy func(pid, ancestor int) bool
}

// NewManager creates a session manager.
func NewManager(tmuxClient *tmux.Client, registry *agents.Registry, cfg *config.Config, poller *status.Poller) *Manager {
	return &Manager{
		tmux:           tmuxClient,
		registry:       registry,
		cfg:            cfg,
		poller:         poller,
		panes:          make(map[string]*TrackedPane),
		counters:       make(map[string]int),
		processOwnedBy: processDescendsFrom,
	}
}

// SpawnAgent spawns a new pane running the named agent.
func (m *Manager) SpawnAgent(ctx context.Context, requester control.Requester, role control.Role, name, dir string) error {
	agent, ok := m.registry.Get(name)
	if !ok {
		return fmt.Errorf("unknown agent type: %q", name)
	}
	return m.spawnPane(ctx, requester, role, "", name, agent.Command, dir)
}

func (m *Manager) SpawnAgentWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, name, dir string) error {
	agent, ok := m.registry.Get(name)
	if !ok {
		return fmt.Errorf("unknown agent type: %q", name)
	}
	return m.spawnPane(ctx, requester, role, windowName, name, agent.Command, dir)
}

// SpawnCommand spawns a pane running an arbitrary command.
func (m *Manager) SpawnCommand(ctx context.Context, requester control.Requester, role control.Role, command, dir string) error {
	return m.spawnPane(ctx, requester, role, "", "", command, dir)
}

func (m *Manager) SpawnCommandWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, command, dir string) error {
	return m.spawnPane(ctx, requester, role, windowName, "", command, dir)
}

func (m *Manager) spawnPane(ctx context.Context, requester control.Requester, role control.Role, windowName, agentType, command, dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkSpawnLimit(requester, role); err != nil {
		return err
	}
	rootID := requester.RootID
	if rootID == "" {
		rootID = requester.PaneID
	}
	spawnCommand := roleCommand(command, role, requester.PaneID, rootID)

	var paneID string
	var err error
	if windowName != "" {
		exists, existsErr := m.tmux.WindowExists(ctx, windowName)
		if existsErr != nil {
			return fmt.Errorf("checking window: %w", existsErr)
		}
		if exists {
			paneID, err = m.tmux.SplitWindowInWindow(ctx, windowName, spawnCommand, dir)
		} else {
			paneID, err = m.tmux.NewWindow(ctx, windowName, spawnCommand, dir)
		}
	} else {
		paneID, err = m.tmux.SplitWindow(ctx, spawnCommand, dir)
	}
	if err != nil {
		return fmt.Errorf("spawning pane: %w", err)
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
		PaneID:     paneID,
		WindowName: windowName,
		AgentType:  agentType,
		AgentName:  displayName,
		Command:    command,
		Status:     status.StatusRunning,
		Role:       role,
		ParentID:   requester.PaneID,
		RootID:     rootID,
	}
	m.panes[paneID] = tracked
	m.stylePaneControl(ctx, tracked)

	if m.poller != nil {
		m.poller.Track(paneID, agentType)
	}

	if windowName == "" {
		_ = m.applyLayout(ctx, m.cfg.Session.DefaultLayout)
	}

	return nil
}

func roleCommand(command string, role control.Role, parentID, rootID string) string {
	return fmt.Sprintf(
		`AGENCY_ROLE=%s AGENCY_PANE_ID="$(tmux display-message -p '#{pane_id}')" AGENCY_PARENT_ID=%q AGENCY_ROOT_ID=%q %s`,
		role, parentID, rootID, command,
	)
}

func (m *Manager) checkSpawnLimit(requester control.Requester, role control.Role) error {
	if max := m.cfg.Session.MaxPanes; max > 0 && len(m.panes) >= max {
		return fmt.Errorf("agency pane limit reached (%d)", max)
	}
	if role == control.RoleManager {
		count := 0
		for _, pane := range m.panes {
			if pane.Role == control.RoleManager {
				count++
			}
		}
		if max := m.cfg.Session.MaxManagers; max > 0 && count >= max {
			return fmt.Errorf("agency manager limit reached (%d)", max)
		}
	}
	if role == control.RoleWorker {
		count := 0
		for _, pane := range m.panes {
			if pane.Role == control.RoleWorker && pane.ParentID == requester.PaneID {
				count++
			}
		}
		if max := m.cfg.Session.MaxWorkersPerManager; max > 0 && count >= max {
			return fmt.Errorf("worker limit reached for %s (%d)", requester.PaneID, max)
		}
	}
	return nil
}

func (m *Manager) stylePaneControl(ctx context.Context, pane *TrackedPane) {
	_ = m.tmux.SetPaneOption(ctx, pane.PaneID, "@agency_role", string(pane.Role))
	_ = m.tmux.SetPaneOption(ctx, pane.PaneID, "@agency_parent", pane.ParentID)
	_ = m.tmux.SetPaneOption(ctx, pane.PaneID, "@agency_root", pane.RootID)
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

func (m *Manager) ResolveRequester(ctx context.Context, pid int) (control.Requester, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return control.Requester{}, err
	}
	for _, pane := range panes {
		if !m.processOwnedBy(pid, pane.PID) {
			continue
		}
		tracked := m.panes[pane.ID]
		if tracked == nil || tracked.Role == "" {
			return control.Requester{}, fmt.Errorf("pane %s has no agency role", pane.ID)
		}
		return control.Requester{PaneID: pane.ID, Role: tracked.Role, RootID: tracked.RootID}, nil
	}
	return control.Requester{}, fmt.Errorf("requester process %d does not belong to an agency pane", pid)
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

func (m *Manager) KillWindow(ctx context.Context, windowName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.tmux.KillWindow(ctx, windowName); err != nil {
		return err
	}
	for id, pane := range m.panes {
		if pane.WindowName != windowName {
			continue
		}
		if m.poller != nil {
			m.poller.Untrack(id)
		}
		delete(m.panes, id)
	}
	return nil
}

func (m *Manager) RenameWindow(ctx context.Context, target, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.tmux.RenameWindow(ctx, target, name); err != nil {
		return err
	}
	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return err
	}
	for _, pane := range panes {
		if tracked, ok := m.panes[pane.ID]; ok {
			tracked.WindowName = pane.WindowName
		}
	}
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
	minColWidth := m.cfg.Session.MinColumnWidth
	if minColWidth <= 0 {
		minColWidth = 90
	}

	// Prefer 2 rows per column when the window is wide enough.
	// Calculate how many columns we'd need with maxRows=2, then check
	// if each column would still be at least minColWidth wide.
	// If not, fall back to the full maxRows (more rows = fewer columns = wider).
	effectiveMax := maxRows
	if maxRows >= 2 {
		colsNeeded := (info.PaneCount + 1) / 2 // ceil(paneCount / 2)
		if colsNeeded < 1 {
			colsNeeded = 1
		}
		// Available width per column: subtract separators, divide evenly.
		widthPerCol := (info.Width - (colsNeeded - 1)) / colsNeeded
		if widthPerCol >= minColWidth {
			effectiveMax = 2
		}
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
	sort.Slice(panes, func(i, j int) bool {
		if panes[i].WindowIndex != panes[j].WindowIndex {
			return panes[i].WindowIndex < panes[j].WindowIndex
		}
		return panes[i].Index < panes[j].Index
	})

	controllerID := ""
	for _, pane := range panes {
		if pane.Role == string(control.RoleController) {
			controllerID = pane.ID
			break
		}
	}
	if controllerID == "" && len(panes) > 0 {
		controllerID = panes[0].ID
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

		role, err := control.ParseRole(pane.Role)
		if err != nil {
			if pane.ID == controllerID {
				role = control.RoleController
			} else {
				role = control.RoleManager
			}
		}
		parentID := pane.ParentID
		rootID := pane.RootID
		if role == control.RoleController {
			parentID = ""
			rootID = pane.ID
		} else {
			if parentID == "" {
				parentID = controllerID
			}
			if rootID == "" {
				rootID = controllerID
			}
		}

		tracked := &TrackedPane{
			PaneID:     pane.ID,
			WindowName: pane.WindowName,
			AgentType:  agentType,
			AgentName:  displayName,
			Command:    pane.Command,
			Status:     status.StatusIdle,
			Role:       role,
			ParentID:   parentID,
			RootID:     rootID,
		}
		m.panes[pane.ID] = tracked
		m.stylePaneControl(ctx, tracked)

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
		if err := m.tmux.SendText(ctx, id, keys, true); err != nil {
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
