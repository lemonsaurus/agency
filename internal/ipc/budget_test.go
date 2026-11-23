package ipc

import (
	"context"
	"strings"
	"testing"

	"github.com/lemonsaurus/agency/internal/budget"
)

func TestStartedTypesTheNoticeWhenTheRequesterHasNoBridge(t *testing.T) {
	handler := &mockHandler{}
	a := NewAdmission(context.Background(), handler, budget.New(budget.DefaultLimits, ""), t.TempDir())
	a.Started(budget.Started{Ticket: budget.Ticket{ID: "q-13", Label: "Deploy", Pane: "%5177"}, PaneID: "%5446"},
		func(string, string) bool { return false })
	if len(handler.sends) != 1 {
		t.Fatalf("sends %+v, want the notice typed into the requester", handler.sends)
	}
	send := handler.sends[0]
	if send.pane != "%5177" || !send.enter || !strings.Contains(send.text, "started as %5446") {
		t.Errorf("send %+v", send)
	}
}

func TestStartedUsesTheBridgeWhenItTakesTheNotice(t *testing.T) {
	handler := &mockHandler{}
	a := NewAdmission(context.Background(), handler, budget.New(budget.DefaultLimits, ""), t.TempDir())
	a.Started(budget.Started{Ticket: budget.Ticket{ID: "q-1", Pane: "%2"}, PaneID: "%3"},
		func(string, string) bool { return true })
	if len(handler.sends) != 0 {
		t.Errorf("sends %+v, want none", handler.sends)
	}
}
