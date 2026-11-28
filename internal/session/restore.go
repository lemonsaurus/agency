package session

import (
	"context"
	"fmt"

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

// Reparent puts a pane under a new parent. Its role must be one the parent can create, and
// every descendant takes the parent's root.
func (m *Manager) Reparent(ctx context.Context, move control.Reparent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pane, ok := m.panes[move.Pane]
	if !ok {
		return fmt.Errorf("no tracked pane %s", move.Pane)
	}
	parent, ok := m.panes[move.Parent]
	if !ok {
		return fmt.Errorf("no tracked pane %s", move.Parent)
	}
	if move.Pane == move.Parent || m.descends(parent, move.Pane) {
		return fmt.Errorf("%s cannot be its own ancestor", move.Pane)
	}
	if _, err := (control.Requester{Role: parent.Role}).ChildRole(pane.Role); err != nil {
		return fmt.Errorf("%s cannot be the parent of %s: %w", move.Parent, move.Pane, err)
	}
	pane.ParentID, pane.RootID = parent.PaneID, parent.RootID
	m.stylePaneControl(ctx, pane)
	m.reroot(ctx, pane)
	return nil
}

// descends is whether pane is below ancestor.
func (m *Manager) descends(pane *TrackedPane, ancestor string) bool {
	for seen := 0; pane != nil && pane.ParentID != "" && seen < len(m.panes); seen++ {
		if pane.ParentID == ancestor {
			return true
		}
		pane = m.panes[pane.ParentID]
	}
	return false
}

// reroot gives every pane below parent its root.
func (m *Manager) reroot(ctx context.Context, parent *TrackedPane) {
	for _, child := range m.panes {
		if child.ParentID == parent.PaneID && child.RootID != parent.RootID {
			child.RootID = parent.RootID
			m.stylePaneControl(ctx, child)
			m.reroot(ctx, child)
		}
	}
}
