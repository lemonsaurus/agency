package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/deaths"
)

func recordedDeaths(t *testing.T, mgr *Manager) []deaths.Event {
	t.Helper()
	events, err := mgr.Deaths.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestDeathLogRecordsKillHandoffAndVanish(t *testing.T) {
	ctx := context.Background()
	mock := &testMock{}
	mgr := newTestManager(mock)
	mgr.Deaths = &deaths.Log{Path: filepath.Join(t.TempDir(), "pane-deaths.jsonl")}
	if err := mgr.SpawnAgent(ctx, testController, control.RoleManager, "claude", "/tmp/manager", "Pack Lane"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SpawnAgent(ctx, testController, control.RoleManager, "claude", "/tmp/other", "Other Lane"); err != nil {
		t.Fatal(err)
	}
	mock.listOutput = "1\twork\t%1\t0\tclaude\t/tmp/manager\t1\t101\tmanager\t%0\t%0\t\n1\twork\t%2\t1\tclaude\t/tmp/other\t0\t102\tmanager\t%0\t%0\t"

	manager := control.Requester{PaneID: "%1", Role: control.RoleManager, RootID: "%0", PID: 4242}
	successor, err := mgr.ReplacePane(ctx, manager, "handoff-pi", "")
	if err != nil {
		t.Fatal(err)
	}
	killer := control.Requester{PaneID: successor, Role: control.RoleManager, RootID: "%0", PID: 4343}
	if err := mgr.KillPane(ctx, killer, "%1"); err != nil {
		t.Fatal(err)
	}
	mock.listOutput = "1\twork\t" + successor + "\t0\tclaude\t/tmp/manager\t1\t103\tmanager\t%0\t%0\t"
	removed, err := mgr.PruneDead(ctx)
	if err != nil || len(removed) != 1 || removed[0] != "%2" {
		t.Fatalf("pruned %v, %v", removed, err)
	}

	events := recordedDeaths(t, mgr)
	if len(events) != 3 {
		t.Fatalf("events = %+v", events)
	}
	handoff, kill, gone := events[0], events[1], events[2]
	if handoff.Kind != deaths.KindHandoff || handoff.Pane != "%1" || handoff.Successor != successor || handoff.PID != 101 || handoff.Label != "Pack Lane" {
		t.Fatalf("handoff = %+v", handoff)
	}
	if kill.Kind != deaths.KindKilled || kill.Pane != "%1" || kill.PID != 101 || kill.Dir != "/tmp/manager" || kill.Role != "manager" ||
		kill.By == nil || kill.By.Pane != successor || kill.By.PID != 4343 {
		t.Fatalf("kill = %+v by %+v", kill, kill.By)
	}
	if gone.Kind != deaths.KindGone || gone.Pane != "%2" || gone.Label != "Other Lane" {
		t.Fatalf("gone = %+v", gone)
	}
}

func TestDeathLogRecordsEveryPaneOfAKilledGroup(t *testing.T) {
	ctx := context.Background()
	mock := &testMock{listOutput: "1\tπ pi@a\t%1\t0\tpi\t/tmp/a\t1\t200\t\t\t\t\t\t@1\t\tLane A\tjournalia\tcloud\n" +
		"2\tπ pi@b\t%2\t0\tpi\t/tmp/b\t1\t201\t\t\t\t\t\t@2\t\tLane B\tjournalia\tcloud\n" +
		"3\tπ pi@c\t%3\t0\tpi\t/tmp/c\t1\t202\t\t\t\t\t\t@3\t\tLane C\tbrood\tcloud"}
	mgr := newTestManager(mock)
	mgr.WindowPerPane = true
	mgr.Deaths = &deaths.Log{Path: filepath.Join(t.TempDir(), "pane-deaths.jsonl")}
	human := control.Requester{Role: control.RoleController, Human: true}
	if err := mgr.KillWindow(ctx, human, "journalia"); err != nil {
		t.Fatal(err)
	}

	events := recordedDeaths(t, mgr)
	if len(events) != 2 || events[0].Pane != "%1" || events[1].Pane != "%2" {
		t.Fatalf("events = %+v", events)
	}
	for _, e := range events {
		if e.Group != "journalia" || e.Note != "kill --window journalia" || e.By == nil || !e.By.Human {
			t.Fatalf("event = %+v by %+v", e, e.By)
		}
	}
}
