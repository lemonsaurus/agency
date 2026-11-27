package session

import (
	"context"

	"github.com/lemonsaurus/agency/internal/control"
)

// RestorePane brings back one pane of a dead session: the same command, role,
// task label and window group, under the parent and root that were restored
// before it. The old pane ID stays on the pane as @agency_restored.
func (m *Manager) RestorePane(ctx context.Context, spec control.RestoreSpec) (string, error) {
	requester := control.Requester{PaneID: spec.Parent, RootID: spec.Root, Human: true}
	paneID, err := m.spawnPane(ctx, requester, spec.Role, spec.Window, m.registry.DetectType(spec.Command), spec.Command, spec.Dir, spec.Label, "")
	if err != nil {
		return "", err
	}
	_ = m.tmux.SetPaneOption(ctx, paneID, "@agency_restored", spec.Old)
	return paneID, nil
}

// Forget drops every tracked pane, for a session whose tmux server is gone.
func (m *Manager) Forget() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.panes {
		if m.poller != nil {
			m.poller.Untrack(id)
		}
		delete(m.panes, id)
	}
	m.counters = make(map[string]int)
}
