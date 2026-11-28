package session

import (
	"context"
	"testing"

	"github.com/lemonsaurus/agency/internal/control"
)

// A pane moves under a parent whose role can create it, and its descendants take the new root;
// a pane cannot move under itself, its own descendant or a pane of the wrong role.
func TestReparentMovesAPaneAndItsDescendants(t *testing.T) {
	m := newTestManager(&testMock{})
	m.panes["%1"] = &TrackedPane{PaneID: "%1", Role: control.RoleController, RootID: "%1"}
	m.panes["%2"] = &TrackedPane{PaneID: "%2", Role: control.RoleController, RootID: "%2"}
	m.panes["%3"] = &TrackedPane{PaneID: "%3", Role: control.RoleManager, ParentID: "", RootID: ""}
	m.panes["%4"] = &TrackedPane{PaneID: "%4", Role: control.RoleWorker, ParentID: "%3", RootID: ""}
	ctx := context.Background()
	if err := m.Reparent(ctx, control.Reparent{Pane: "%3", Parent: "%2"}); err != nil {
		t.Fatal(err)
	}
	if got := m.panes["%3"]; got.ParentID != "%2" || got.RootID != "%2" || m.panes["%4"].RootID != "%2" {
		t.Fatalf("after move: %+v %+v", got, m.panes["%4"])
	}
	for _, bad := range []control.Reparent{{Pane: "%3", Parent: "%3"}, {Pane: "%3", Parent: "%4"}, {Pane: "%1", Parent: "%2"}, {Pane: "%3", Parent: "%9"}} {
		if err := m.Reparent(ctx, bad); err == nil {
			t.Errorf("Reparent(%+v) succeeded", bad)
		}
	}
}
