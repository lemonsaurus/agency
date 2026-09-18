package session

import (
	"context"
	"fmt"
	"os"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/status"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// offlineViewer marks the placeholder pane shown when the host is unreachable.
const offlineViewer = "offline"

// SyncCloud reconciles the cloud-harness window with the remote server: one
// viewer pane per remote window, viewers of vanished windows removed. Remote
// state is never touched. When the host is unreachable and nothing is shown
// yet, one offline placeholder keeps the window present.
func (m *Manager) SyncCloud(ctx context.Context) (string, error) {
	if m.cfg.Cloud.Host == "" {
		return "", fmt.Errorf("no [cloud] host configured")
	}
	remote := &cloud.Client{Host: m.cfg.Cloud.Host}
	windows, remoteErr := remote.Windows(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return "", err
	}
	viewers := map[string]string{} // remote window id → local pane id
	for _, pane := range panes {
		if pane.CloudWindow != "" {
			viewers[pane.CloudWindow] = pane.ID
		}
	}

	if remoteErr != nil {
		if len(viewers) == 0 {
			m.spawnViewer(ctx, cloud.Window{ID: offlineViewer, Name: "☁ cloud offline"}, "")
		}
		return "", remoteErr
	}

	added, removed := 0, 0
	present := map[string]bool{}
	for _, window := range windows {
		present[window.ID] = true
		paneID := viewers[window.ID]
		if paneID == "" {
			paneID = m.spawnViewer(ctx, window, m.registry.DetectType(window.Pane.Command))
			added++
		}
		// Mirror the pending badge so the border shows the Prefix+P hint.
		_ = m.tmux.SetPaneOption(ctx, paneID, "@agency_promotion", window.Pane.PendingPromotion)
	}
	for id, paneID := range viewers {
		if present[id] {
			continue
		}
		if err := m.tmux.KillPane(ctx, paneID); err != nil {
			continue
		}
		if m.poller != nil {
			m.poller.Untrack(paneID)
		}
		delete(m.panes, paneID)
		removed++
	}
	if added > 0 || removed > 0 {
		_ = m.applyLayoutForWindow(ctx, m.cfg.Session.DefaultLayout, m.tmux.SessionName+":"+cloud.WindowName)
	}
	return fmt.Sprintf("%d cloud panes, %d added, %d removed", len(windows), added, removed), nil
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
		Command:    pane.Command,
		Status:     status.StatusIdle,
	}
	if m.poller != nil {
		m.poller.Track(pane.ID, agentType)
	}
}

// spawnViewer opens a local pane that attaches to one remote window. The pane
// has no local role: nothing inside it may call the local daemon.
func (m *Manager) spawnViewer(ctx context.Context, window cloud.Window, agentType string) string {
	bin, err := os.Executable()
	if err != nil {
		bin = "agency"
	}
	command := fmt.Sprintf("%s cloud-view %s", bin, window.ID)
	exists, err := m.tmux.WindowExists(ctx, cloud.WindowName)
	if err != nil {
		return ""
	}
	var paneID string
	if exists {
		paneID, err = m.tmux.SplitWindowInWindow(ctx, cloud.WindowName, command, "")
	} else {
		paneID, err = m.tmux.NewWindow(ctx, cloud.WindowName, command, "")
	}
	if err != nil {
		return ""
	}
	color := paneColors[m.colorIndex%len(paneColors)]
	m.colorIndex++
	m.stylePaneLabel(ctx, paneID, window.Name, color)
	_ = m.tmux.SetPaneOption(ctx, paneID, "@agency_cloud", window.ID)
	m.panes[paneID] = &TrackedPane{
		PaneID:     paneID,
		WindowName: cloud.WindowName,
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
