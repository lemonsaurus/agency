package session

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/status"
	"github.com/lemonsaurus/agency/internal/tmux"
)

type cloudClient interface {
	Windows(context.Context) ([]cloud.Window, error)
	Run(context.Context, time.Duration, ...string) (string, error)
}

// SyncCloud reconciles the remote world with the remote server: one viewer
// pane per remote window, in the window named after its group. The remote
// group wins, so viewers moved or renamed elsewhere follow it. Remote state is
// never touched. A placeholder keeps the remote world open while it shows no
// agents, whether the host is empty or unreachable.
func (m *Manager) SyncCloud(ctx context.Context) (string, error) {
	if m.cfg.Cloud.Host == "" {
		return "", fmt.Errorf("no [cloud] host configured")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	windows, remoteErr := m.cloud.Windows(ctx)
	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return "", err
	}
	viewers := map[string]tmux.PaneInfo{} // remote window id → local viewer
	var placeholders []string
	for _, pane := range panes {
		switch pane.CloudWindow {
		case "":
		case cloud.Placeholder:
			placeholders = append(placeholders, pane.ID)
		default:
			viewers[pane.CloudWindow] = pane
		}
	}

	if remoteErr != nil {
		if len(viewers) == 0 && len(placeholders) == 0 {
			m.spawnViewer(ctx, cloud.Window{ID: cloud.Placeholder}, "")
		}
		return "", remoteErr
	}
	// The placeholder goes in before the last viewer leaves, so the session
	// never empties.
	if len(windows) == 0 && len(placeholders) == 0 {
		m.spawnViewer(ctx, cloud.Window{ID: cloud.Placeholder}, "")
	}

	added, moved, removed := 0, 0, 0
	present := map[string]bool{}
	for _, window := range windows {
		present[window.ID] = true
		group := cloud.Group(window.Pane)
		viewer, ok := viewers[window.ID]
		paneID := viewer.ID
		switch {
		case !ok:
			paneID = m.spawnViewer(ctx, window, m.registry.DetectType(window.Pane.Command))
			if paneID == "" {
				return "", fmt.Errorf("creating cloud viewer for %s", window.ID)
			}
			added++
		case viewer.Session != m.remote.SessionName || viewer.WindowName != group:
			// Only a new viewer can open a missing session; viewers hold no state.
			if !m.remote.SessionExists(ctx) {
				m.dropViewer(ctx, paneID)
				if paneID = m.spawnViewer(ctx, window, m.registry.DetectType(window.Pane.Command)); paneID == "" {
					return "", fmt.Errorf("creating cloud viewer for %s", window.ID)
				}
			} else if err := m.placeViewer(ctx, paneID, group); err != nil {
				return "", err
			}
			if tracked := m.panes[paneID]; tracked != nil {
				tracked.WindowName = group
			}
			_ = m.applyLayoutForWindow(ctx, m.cfg.Session.DefaultLayout, viewer.WindowID)
			_ = m.applyLayoutForWindow(ctx, m.cfg.Session.DefaultLayout, paneID)
			moved++
		}
		if err := m.mirrorViewer(ctx, paneID, window.Pane); err != nil {
			return "", err
		}
	}
	for id, viewer := range viewers {
		if present[id] {
			continue
		}
		m.dropViewer(ctx, viewer.ID)
		_ = m.applyLayoutForWindow(ctx, m.cfg.Session.DefaultLayout, viewer.WindowID)
		removed++
	}
	if len(windows) > 0 {
		for _, paneID := range placeholders {
			m.dropViewer(ctx, paneID)
		}
	}
	return fmt.Sprintf("%d cloud panes, %d added, %d moved, %d removed", len(windows), added, moved, removed), nil
}

func (m *Manager) dropViewer(ctx context.Context, paneID string) {
	if err := m.tmux.KillPane(ctx, paneID); err != nil {
		return
	}
	if m.poller != nil {
		m.poller.Untrack(paneID)
	}
	delete(m.panes, paneID)
}

// placeViewer moves a viewer into the remote world's window for group.
func (m *Manager) placeViewer(ctx context.Context, paneID, group string) error {
	windowID, err := m.remote.WindowID(ctx, group)
	if err != nil {
		return err
	}
	if windowID == "" {
		_, err = m.tmux.Cmd.Run(ctx, "break-pane", "-d", "-s", paneID, "-n", group, "-t", m.remote.SessionName+":")
		return err
	}
	out, err := m.tmux.Cmd.Run(ctx, "join-pane", "-d", "-s", paneID, "-t", windowID)
	if err != nil && strings.Contains(err.Error()+out, "no space for a new pane") {
		if _, layoutErr := m.tmux.Cmd.Run(ctx, "select-layout", "-t", windowID, "tiled"); layoutErr == nil {
			_, err = m.tmux.Cmd.Run(ctx, "join-pane", "-d", "-s", paneID, "-t", windowID)
		}
	}
	return err
}

// openViewer starts a viewer process in the remote world's window for group,
// creating the session and the window as needed. The session hands its
// clients back to the local world when it closes.
func (m *Manager) openViewer(ctx context.Context, group, command string) (string, error) {
	if !m.remote.SessionExists(ctx) {
		paneID, err := m.remote.NewSessionWindow(ctx, group, command)
		if err == nil {
			_, err = m.tmux.Cmd.Run(ctx, "set-option", "-t", m.remote.SessionName, "detach-on-destroy", "off")
		}
		return paneID, err
	}
	windowID, err := m.remote.WindowID(ctx, group)
	if err != nil {
		return "", err
	}
	if windowID == "" {
		return m.remote.NewWindow(ctx, group, command, "")
	}
	paneID, err := m.remote.SplitWindowAt(ctx, windowID, command, "")
	if err == nil {
		_ = m.applyLayoutForWindow(ctx, m.cfg.Session.DefaultLayout, paneID)
	}
	return paneID, err
}

// renameRemoteWindow renames a remote-world window together with the remote
// group behind it. Only window ids reach the remote world; names stay local.
// It reports false when target is not a remote-world window.
func (m *Manager) renameRemoteWindow(ctx context.Context, target, name string) (bool, error) {
	if m.cfg.Cloud.Host == "" {
		return false, nil
	}
	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return false, err
	}
	for _, pane := range panes {
		if pane.Session != m.remote.SessionName || pane.WindowID != target {
			continue
		}
		if _, err := m.cloud.Run(ctx, 15*time.Second, "rename-window", pane.WindowName, name); err != nil {
			return true, err
		}
		return true, m.remote.RenameWindow(ctx, pane.WindowID, name)
	}
	return false, nil
}

// killGroup kills every agent in group name.
func (m *Manager) killGroup(ctx context.Context, name string) error {
	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return err
	}
	killed := map[string]bool{} // grouped view sessions list each pane again
	for _, pane := range panes {
		if cloud.Group(pane) != name || killed[pane.ID] {
			continue
		}
		if err := m.tmux.KillPane(ctx, pane.ID); err != nil {
			return err
		}
		if m.poller != nil {
			m.poller.Untrack(pane.ID)
		}
		delete(m.panes, pane.ID)
		killed[pane.ID] = true
	}
	if len(killed) == 0 {
		return fmt.Errorf("window %q not found", name)
	}
	return nil
}

// renameGroup moves every agent of group target into group name. Ungrouped
// agents belong to the default group.
func (m *Manager) renameGroup(ctx context.Context, target, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("window name is required")
	}
	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, pane := range panes {
		if cloud.Group(pane) != target {
			continue
		}
		if err := m.tmux.SetPaneOption(ctx, pane.ID, "@agency_group", name); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return fmt.Errorf("window %q not found", target)
	}
	return nil
}

func (m *Manager) labelViewer(ctx context.Context, paneID, windowID, label string) (string, error) {
	if m.cfg.Cloud.Host == "" || windowID == cloud.Placeholder {
		return "", fmt.Errorf("cloud pane is unavailable")
	}
	windows, err := m.cloud.Windows(ctx)
	if err != nil {
		return "", err
	}
	for _, window := range windows {
		if window.ID != windowID {
			continue
		}
		if _, err := m.cloud.Run(ctx, 15*time.Second, "label", "--pane", window.Pane.ID, "--", label); err != nil {
			return "", err
		}
		window.Pane.TaskLabel = label
		if err := m.mirrorViewer(ctx, paneID, window.Pane); err != nil {
			return "", err
		}
		return label, nil
	}
	return "", fmt.Errorf("cloud window %s is no longer available", windowID)
}

func (m *Manager) mirrorViewer(ctx context.Context, paneID string, remote tmux.PaneInfo) error {
	for _, option := range [][2]string{
		{"@agency_label", folderLabel(remote.CWD)},
		{"@agency_task_label", remote.TaskLabel},
		{"@agency_promotion", remote.PendingPromotion},
	} {
		if err := m.tmux.SetPaneOption(ctx, paneID, option[0], option[1]); err != nil {
			return err
		}
	}
	if pane := m.panes[paneID]; pane != nil {
		pane.TaskLabel = remote.TaskLabel
	}
	return nil
}

// adoptViewer re-tracks a viewer pane after a daemon restart. Its label and
// color are already in pane options.
func (m *Manager) adoptViewer(pane tmux.PaneInfo) {
	agentType := m.registry.DetectType(pane.Command)
	m.panes[pane.ID] = &TrackedPane{
		PaneID:     pane.ID,
		WindowName: pane.WindowName,
		AgentType:  agentType,
		AgentName:  pane.CloudWindow,
		TaskLabel:  pane.TaskLabel,
		Command:    pane.Command,
		Status:     status.StatusIdle,
	}
	if m.poller != nil {
		m.poller.Track(pane.ID, agentType)
	}
}

// spawnViewer opens a remote-world pane that attaches to one remote window.
// The pane has no local role: nothing inside it may call the local daemon.
func (m *Manager) spawnViewer(ctx context.Context, window cloud.Window, agentType string) string {
	bin, err := os.Executable()
	if err != nil {
		bin = "agency"
	}
	command := fmt.Sprintf("%s cloud-view %s", bin, window.ID)
	group := cloud.Group(window.Pane)
	paneID, err := m.openViewer(ctx, group, command)
	if err != nil || paneID == "" {
		return ""
	}
	color := paneColors[m.colorIndex%len(paneColors)]
	m.colorIndex++
	border := folderLabel(window.Pane.CWD)
	if window.ID == cloud.Placeholder {
		border = "☁ sky"
	}
	m.stylePaneLabel(ctx, paneID, border, color)
	_ = m.tmux.SetPaneOption(ctx, paneID, "@agency_cloud", window.ID)
	m.panes[paneID] = &TrackedPane{
		PaneID:     paneID,
		WindowName: group,
		AgentType:  agentType,
		AgentName:  window.Name,
		Command:    command,
		Status:     status.StatusRunning,
	}
	if m.poller != nil {
		m.poller.Track(paneID, agentType)
	}
	return paneID
}
