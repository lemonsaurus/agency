package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/lemonsaurus/agency/internal/agents"
	"github.com/lemonsaurus/agency/internal/cloud"
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
	PaneID           string
	WindowName       string
	AgentType        string // empty for custom commands
	AgentName        string // display name like "🔒 claudejail@myproject"
	Command          string
	Status           string
	Role             control.Role
	ParentID         string
	RootID           string
	PendingPromotion string
	TaskLabel        string
}

// Manager tracks panes, handles spawn/kill, and satisfies ipc.Handler.
type Manager struct {
	mu       sync.Mutex
	tmux     *tmux.Client
	registry *agents.Registry
	cfg      *config.Config
	poller   *status.Poller
	cloud    cloudClient

	panes          map[string]*TrackedPane // keyed by pane ID
	counters       map[string]int          // instance counters per agent type (fallback when no dir)
	colorIndex     int                     // cycles through paneColors
	currentLayout  string                  // last applied layout name (for relayout)
	processOwnedBy func(pid, ancestor int) bool

	// WindowPerPane gives every spawn its own window, named after the pane
	// label. The headless cloud server runs this way so viewers can attach
	// to exactly one agent.
	WindowPerPane bool
	// OutsideIsHuman grants human authority to requests from processes that
	// belong to no pane. On the cloud box every agent lives in a pane and only
	// Lemon's SSH key reaches the host, so outside means Lemon.
	OutsideIsHuman bool
}

// NewManager creates a session manager.
func NewManager(tmuxClient *tmux.Client, registry *agents.Registry, cfg *config.Config, poller *status.Poller) *Manager {
	return &Manager{
		tmux:           tmuxClient,
		registry:       registry,
		cfg:            cfg,
		poller:         poller,
		cloud:          &cloud.Client{Host: cfg.Cloud.Host},
		panes:          make(map[string]*TrackedPane),
		counters:       make(map[string]int),
		processOwnedBy: processDescendsFrom,
	}
}

// SpawnAgent spawns a new pane running the named agent.
func (m *Manager) SpawnAgent(ctx context.Context, requester control.Requester, role control.Role, name, dir, label string) error {
	agent, ok := m.registry.Get(name)
	if !ok {
		return fmt.Errorf("unknown agent type: %q", name)
	}
	return m.spawnPane(ctx, requester, role, "", name, agent.Command, dir, label)
}

func (m *Manager) SpawnAgentWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, name, dir, label string) error {
	agent, ok := m.registry.Get(name)
	if !ok {
		return fmt.Errorf("unknown agent type: %q", name)
	}
	return m.spawnPane(ctx, requester, role, windowName, name, agent.Command, dir, label)
}

// SpawnCommand spawns a pane running an arbitrary command.
func (m *Manager) SpawnCommand(ctx context.Context, requester control.Requester, role control.Role, command, dir, label string) error {
	return m.spawnPane(ctx, requester, role, "", "", command, dir, label)
}

func (m *Manager) SpawnCommandWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, command, dir, label string) error {
	return m.spawnPane(ctx, requester, role, windowName, "", command, dir, label)
}

func (m *Manager) spawnPane(ctx context.Context, requester control.Requester, role control.Role, windowName, agentType, command, dir, label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := control.ValidateTaskLabel(label, !requester.Human); err != nil {
		return err
	}
	if err := m.checkSpawnLimit(requester, role); err != nil {
		return err
	}
	parentID := requester.PaneID
	rootID := requester.RootID
	if role == control.RoleController {
		parentID = ""
		rootID = ""
	} else if rootID == "" {
		rootID = requester.PaneID
	}
	spawnCommand := roleCommand(command, role, parentID, rootID)
	displayName := m.displayName(agentType, command, dir)

	var paneID string
	var err error
	if m.WindowPerPane {
		windowName = displayName
		paneID, err = m.tmux.NewWindow(ctx, windowName, spawnCommand, dir)
	} else if windowName != "" {
		exists, existsErr := m.tmux.WindowExists(ctx, windowName)
		if existsErr != nil {
			return fmt.Errorf("checking window: %w", existsErr)
		}
		if exists {
			paneID, err = m.tmux.SplitWindowInWindow(ctx, windowName, spawnCommand, dir)
		} else {
			paneID, err = m.tmux.NewWindow(ctx, windowName, spawnCommand, dir)
		}
	} else if requester.Human && role == control.RoleController {
		paneID, windowName, err = m.tmux.SplitWindowInFirstWindow(ctx, spawnCommand, dir)
	} else {
		paneID, err = m.tmux.SplitWindow(ctx, spawnCommand, dir)
	}
	if err != nil {
		return fmt.Errorf("spawning pane: %w", err)
	}
	if err := m.tmux.SetPaneOption(ctx, paneID, "@agency_task_label", label); err != nil {
		_ = m.tmux.KillPane(ctx, paneID)
		return fmt.Errorf("storing task label: %w", err)
	}
	if role == control.RoleController {
		rootID = paneID
	}

	// Pick the next unique color from the palette.
	color := paneColors[m.colorIndex%len(paneColors)]
	m.colorIndex++

	m.stylePaneLabel(ctx, paneID, folderLabel(dir), color)

	tracked := &TrackedPane{
		PaneID:     paneID,
		WindowName: windowName,
		AgentType:  agentType,
		AgentName:  displayName,
		TaskLabel:  label,
		Command:    command,
		Status:     status.StatusRunning,
		Role:       role,
		ParentID:   parentID,
		RootID:     rootID,
	}
	m.panes[paneID] = tracked
	m.stylePaneControl(ctx, tracked)

	if m.poller != nil {
		m.poller.Track(paneID, agentType)
	}

	_ = m.applyLayoutForWindow(ctx, m.cfg.Session.DefaultLayout, paneID)

	return nil
}

// displayName builds the label "icon agenttype@foldername", or falls back to
// a per-type counter when there is no directory.
func (m *Manager) displayName(agentType, command, dir string) string {
	folder := folderLabel(dir)
	if agentType != "" {
		agent, _ := m.registry.Get(agentType)
		if folder != "" {
			return fmt.Sprintf("%s %s@%s", agent.Icon, agentType, folder)
		}
		m.counters[agentType]++
		return fmt.Sprintf("%s %s #%d", agent.Icon, agentType, m.counters[agentType])
	}
	if folder != "" {
		return fmt.Sprintf(">_ %s@%s", filepath.Base(command), folder)
	}
	m.counters["terminal"]++
	return fmt.Sprintf(">_ terminal #%d", m.counters["terminal"])
}

func roleCommand(command string, role control.Role, parentID, rootID string) string {
	rootValue := fmt.Sprintf("%q", rootID)
	if role == control.RoleController {
		rootValue = `"$(tmux display-message -p '#{pane_id}')"`
	}
	return fmt.Sprintf(
		`AGENCY_ROLE=%s AGENCY_PANE_ID="$(tmux display-message -p '#{pane_id}')" AGENCY_PARENT_ID=%q AGENCY_ROOT_ID=%s %s`,
		role, parentID, rootValue, command,
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
	_ = m.tmux.SetPaneOption(ctx, pane.PaneID, "@agency_command", base64.RawStdEncoding.EncodeToString([]byte(pane.Command)))
	pending := ""
	if pane.PendingPromotion != "" {
		pending = base64.RawStdEncoding.EncodeToString([]byte(pane.PendingPromotion))
	}
	_ = m.tmux.SetPaneOption(ctx, pane.PaneID, "@agency_promotion", pending)
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

func (m *Manager) Capabilities() control.Capabilities {
	prefix := strings.ReplaceAll(m.cfg.Keys.Prefix, "C-", "Ctrl+")
	key := m.cfg.Keys.ApprovePromotion
	if len(key) == 1 && key >= "A" && key <= "Z" {
		key = "Shift+" + key
	}
	return control.Capabilities{Protocol: 1, PaneLabels: true, PromotionShortcut: prefix + " then " + key}
}

func (m *Manager) TaskLabel(ctx context.Context, requester control.Requester, paneID string, label *string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if paneID == "" {
		paneID = requester.PaneID
	}
	if paneID == "" {
		return "", fmt.Errorf("no current agency pane; specify --pane")
	}
	pane := m.panes[paneID]
	if pane == nil {
		return "", fmt.Errorf("pane %s is not tracked", paneID)
	}
	if label == nil {
		return pane.TaskLabel, nil
	}
	if !requester.CanLabelPane(paneID) {
		return "", fmt.Errorf("worker panes may only label their own pane")
	}
	if err := control.ValidateTaskLabel(*label, false); err != nil {
		return "", err
	}
	info, err := m.paneInfo(ctx, paneID)
	if err != nil {
		return "", err
	}
	if info.CloudWindow != "" {
		return m.labelViewer(ctx, paneID, info.CloudWindow, *label)
	}
	if err := m.tmux.SetPaneOption(ctx, paneID, "@agency_task_label", *label); err != nil {
		return "", err
	}
	pane.TaskLabel = *label
	return pane.TaskLabel, nil
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
	// Keybindings, hooks, and popups run as children of the tmux server
	// rather than of any pane. They act for the human: controller authority,
	// whether or not a controller pane is still alive.
	serverPID, err := m.tmux.ServerPID(ctx)
	if m.OutsideIsHuman || err == nil && m.processOwnedBy(pid, serverPID) {
		for _, pane := range panes {
			tracked := m.panes[pane.ID]
			if tracked != nil && tracked.Role == control.RoleController {
				return control.Requester{PaneID: pane.ID, Role: control.RoleController, RootID: tracked.RootID, Human: true}, nil
			}
		}
		return control.Requester{Role: control.RoleController, Human: true}, nil
	}
	return control.Requester{}, fmt.Errorf("requester process %d does not belong to an agency pane", pid)
}

// ReplacePane starts a role-preserving successor beside the requester.
// The caller retires the old pane after confirming the successor started.
func (m *Manager) ReplacePane(ctx context.Context, requester control.Requester, command, dir string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if requester.Human || requester.PaneID == "" {
		return "", fmt.Errorf("replacement must be requested from an agency pane")
	}
	old := m.panes[requester.PaneID]
	if old == nil || old.Role != requester.Role {
		return "", fmt.Errorf("pane %s is not tracked with role %s", requester.PaneID, requester.Role)
	}
	paneInfo, err := m.paneInfo(ctx, old.PaneID)
	if err != nil {
		return "", err
	}

	rootID := old.RootID
	replacementCommand := old.Command
	if command != "" {
		replacementCommand = command
	}
	replacementDir := paneInfo.CWD
	if dir != "" {
		replacementDir = dir
	}
	spawnCommand := roleCommand(replacementCommand, old.Role, old.ParentID, rootID)
	paneID, err := m.tmux.SplitWindowAt(ctx, old.PaneID, spawnCommand, replacementDir)
	if err != nil {
		return "", fmt.Errorf("spawning replacement: %w", err)
	}
	if err := m.tmux.SetPaneOption(ctx, paneID, "@agency_task_label", old.TaskLabel); err != nil {
		_ = m.tmux.KillPane(ctx, paneID)
		return "", fmt.Errorf("storing replacement task label: %w", err)
	}
	if old.Role == control.RoleController {
		rootID = paneID
	}

	color := paneColors[m.colorIndex%len(paneColors)]
	m.colorIndex++
	replacement := &TrackedPane{
		PaneID:           paneID,
		WindowName:       paneInfo.WindowName,
		AgentType:        old.AgentType,
		AgentName:        old.AgentName,
		TaskLabel:        old.TaskLabel,
		Command:          old.Command,
		Status:           status.StatusRunning,
		Role:             old.Role,
		ParentID:         old.ParentID,
		RootID:           rootID,
		PendingPromotion: old.PendingPromotion,
	}
	if replacement.Role == control.RoleController {
		replacement.ParentID = ""
	}
	m.panes[paneID] = replacement
	m.stylePaneLabel(ctx, paneID, folderLabel(replacementDir), color)
	m.stylePaneControl(ctx, replacement)

	for _, pane := range m.panes {
		changed := false
		if pane.PaneID != paneID && pane.ParentID == old.PaneID {
			pane.ParentID = paneID
			changed = true
		}
		if old.Role == control.RoleController && pane.PaneID != old.PaneID && pane.RootID == old.PaneID {
			pane.RootID = paneID
			changed = true
		}
		if changed {
			m.stylePaneControl(ctx, pane)
		}
	}
	if m.poller != nil {
		m.poller.Track(paneID, replacement.AgentType)
	}
	_ = m.applyLayoutForWindow(ctx, m.cfg.Session.DefaultLayout, paneID)
	return paneID, nil
}

func (m *Manager) paneInfo(ctx context.Context, paneID string) (tmux.PaneInfo, error) {
	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return tmux.PaneInfo{}, err
	}
	for _, pane := range panes {
		if pane.ID == paneID {
			return pane, nil
		}
	}
	return tmux.PaneInfo{}, fmt.Errorf("pane %s not found", paneID)
}

// RequestPromotion records a worker's request and notifies its root controller.
func (m *Manager) RequestPromotion(ctx context.Context, requester control.Requester, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if requester.Human || requester.Role != control.RoleWorker {
		return fmt.Errorf("only worker panes can request promotion")
	}
	if reason == "" {
		return fmt.Errorf("promotion reason is required")
	}
	worker := m.panes[requester.PaneID]
	if worker == nil || worker.Role != control.RoleWorker {
		return fmt.Errorf("worker pane %s is not tracked", requester.PaneID)
	}
	root := m.panes[worker.RootID]
	if root == nil || root.Role != control.RoleController {
		return fmt.Errorf("root controller %s is not available", worker.RootID)
	}

	worker.PendingPromotion = reason
	encoded := base64.RawStdEncoding.EncodeToString([]byte(reason))
	if err := m.tmux.SetPaneOption(ctx, worker.PaneID, "@agency_promotion", encoded); err != nil {
		worker.PendingPromotion = ""
		return err
	}
	notice, _ := json.Marshal(struct {
		Type     string `json:"type"`
		PaneID   string `json:"paneId"`
		Reason   string `json:"reason"`
		Shortcut string `json:"approvalShortcut"`
	}{Type: "promotion-request", PaneID: worker.PaneID, Reason: reason, Shortcut: m.Capabilities().PromotionShortcut})
	if err := m.tmux.SendText(ctx, root.PaneID, "[agency] "+string(notice), true); err != nil {
		worker.PendingPromotion = ""
		_ = m.tmux.SetPaneOption(ctx, worker.PaneID, "@agency_promotion", "")
		return err
	}
	return nil
}

// ApprovePromotion promotes a pending worker. Approval is reserved for tmux's human authority.
func (m *Manager) ApprovePromotion(ctx context.Context, requester control.Requester, paneID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !requester.Human {
		return fmt.Errorf("promotion approval requires human tmux authority")
	}
	worker := m.panes[paneID]
	if worker == nil || worker.Role != control.RoleWorker {
		return fmt.Errorf("pane %s is not a worker", paneID)
	}
	if worker.PendingPromotion == "" {
		return fmt.Errorf("pane %s has no pending promotion", paneID)
	}
	root := m.panes[worker.RootID]
	if root == nil || root.Role != control.RoleController {
		return fmt.Errorf("root controller %s is not available", worker.RootID)
	}
	parent := m.panes[worker.ParentID]
	if parent == nil || (parent.Role != control.RoleManager && parent.PaneID != root.PaneID) {
		return fmt.Errorf("promotion parent %s is not available", worker.ParentID)
	}
	managerCount := 0
	for _, pane := range m.panes {
		if pane.Role == control.RoleManager {
			managerCount++
		}
	}
	if max := m.cfg.Session.MaxManagers; max > 0 && managerCount >= max {
		return fmt.Errorf("agency manager limit reached (%d)", max)
	}

	parentInfo, err := m.paneInfo(ctx, parent.PaneID)
	if err != nil {
		return err
	}
	workerInfo, err := m.paneInfo(ctx, worker.PaneID)
	if err != nil {
		return err
	}
	if workerInfo.WindowName != parentInfo.WindowName {
		if err := m.tmux.MovePane(ctx, worker.PaneID, parentInfo.WindowName); err != nil {
			return err
		}
	}
	worker.Role = control.RoleManager
	worker.ParentID = root.PaneID
	worker.RootID = root.PaneID
	worker.WindowName = parentInfo.WindowName
	worker.PendingPromotion = ""
	m.stylePaneControl(ctx, worker)
	return nil
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

// SendText delivers text to a pane, prefixed with the sender's pane id
// and role so agents can tell relayed messages from human keystrokes.
// The requester is resolved server-side from the caller's PID, so the
// attribution cannot be forged or omitted.
func (m *Manager) SendText(ctx context.Context, requester control.Requester, paneID, text string, enter bool) error {
	label := string(requester.Role)
	if requester.PaneID != "" {
		label = requester.PaneID + " " + label
	}
	return m.tmux.SendText(ctx, paneID, fmt.Sprintf("[from %s] %s", label, text), enter)
}

// MovePane moves a pane into the named window.
func (m *Manager) MovePane(ctx context.Context, paneID, windowName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.tmux.MovePane(ctx, paneID, windowName); err != nil {
		return err
	}
	if tracked, ok := m.panes[paneID]; ok {
		tracked.WindowName = windowName
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

// PruneDead removes tracked panes that no longer exist in tmux, so the
// poller stops chasing ghosts and spawn limits stay accurate. Returns the
// pruned pane IDs.
func (m *Manager) PruneDead(ctx context.Context) ([]string, error) {
	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return nil, err
	}
	alive := make(map[string]bool, len(panes))
	for _, pane := range panes {
		alive[pane.ID] = true
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	var removed []string
	for id := range m.panes {
		if alive[id] {
			continue
		}
		if m.poller != nil {
			m.poller.Untrack(id)
		}
		delete(m.panes, id)
		removed = append(removed, id)
	}
	return removed, nil
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
	return m.applyLayoutForWindow(ctx, name, m.tmux.SessionName)
}

func (m *Manager) applyLayoutForWindow(ctx context.Context, name, target string) error {
	if name == "tiled" {
		return m.applyCustomTiledForWindow(ctx, target)
	}
	return m.tmux.SelectLayoutForWindow(ctx, target, tmuxLayout(name))
}

func (m *Manager) applyCustomTiledForWindow(ctx context.Context, target string) error {
	info, err := m.tmux.GetWindowInfoForWindow(ctx, target)
	if err != nil {
		return m.tmux.SelectLayoutForWindow(ctx, target, "tiled")
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
	return m.tmux.SelectLayoutForWindow(ctx, target, layoutStr)
}

// MigrateLegacyRoles assigns the selected controller and preserves other pane roles.
func (m *Manager) MigrateLegacyRoles(ctx context.Context, controllerID string) error {
	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return err
	}
	byID := make(map[string]tmux.PaneInfo, len(panes))
	for _, pane := range panes {
		if _, err := control.ParseRole(pane.Role); err != nil {
			return fmt.Errorf("pane %s: %w", pane.ID, err)
		}
		if pane.Role == string(control.RoleController) && pane.ID != controllerID {
			return fmt.Errorf("session already has controller %s", pane.ID)
		}
		byID[pane.ID] = pane
	}
	controller, ok := byID[controllerID]
	if !ok || controller.Role == string(control.RoleWorker) || (controller.ParentID != "" && controller.ParentID != controllerID) {
		return fmt.Errorf("select a legacy root manager as controller")
	}
	for i := range panes {
		pane := &panes[i]
		pane.RootID = controllerID
		if pane.ID == controllerID {
			pane.Role = string(control.RoleController)
			pane.ParentID = ""
		} else if pane.Role == string(control.RoleManager) && pane.ParentID == "" {
			pane.ParentID = controllerID
		} else if parent, ok := byID[pane.ParentID]; !ok || parent.Role == string(control.RoleWorker) || parent.ID == pane.ID {
			return fmt.Errorf("pane %s has invalid parent %s", pane.ID, pane.ParentID)
		}
	}
	for _, pane := range panes {
		for _, option := range [][2]string{{"@agency_parent", pane.ParentID}, {"@agency_root", pane.RootID}, {"@agency_role", pane.Role}} {
			if err := m.tmux.SetPaneOption(ctx, pane.ID, option[0], option[1]); err != nil {
				return err
			}
		}
	}
	return nil
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
		for _, pane := range panes {
			if pane.Role == string(control.RoleManager) && (pane.ParentID == "" || pane.ParentID == pane.ID) {
				return fmt.Errorf("legacy session has no controller; relaunch with agency --migrate-controller <root-pane>")
			}
		}
		controllerID = panes[0].ID
	}

	for _, pane := range panes {
		if _, exists := m.panes[pane.ID]; exists {
			continue
		}
		if pane.CloudWindow != "" {
			m.adoptViewer(pane)
			continue
		}

		color := paneColors[m.colorIndex%len(paneColors)]
		m.colorIndex++

		agencyCommand := pane.Command
		if pane.AgencyCommand != "" {
			if decoded, decodeErr := base64.RawStdEncoding.DecodeString(pane.AgencyCommand); decodeErr == nil {
				agencyCommand = string(decoded)
			}
		}
		agentType := m.registry.DetectType(agencyCommand)
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

		m.stylePaneLabel(ctx, pane.ID, folderLabel(pane.CWD), color)

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

		pendingPromotion := ""
		if pane.PendingPromotion != "" {
			if decoded, decodeErr := base64.RawStdEncoding.DecodeString(pane.PendingPromotion); decodeErr == nil {
				pendingPromotion = string(decoded)
			}
		}
		tracked := &TrackedPane{
			PaneID:           pane.ID,
			WindowName:       pane.WindowName,
			AgentType:        agentType,
			AgentName:        displayName,
			TaskLabel:        pane.TaskLabel,
			Command:          agencyCommand,
			Status:           status.StatusIdle,
			Role:             role,
			ParentID:         parentID,
			RootID:           rootID,
			PendingPromotion: pendingPromotion,
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
