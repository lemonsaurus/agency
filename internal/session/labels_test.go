package session

import (
	"context"
	"strings"
	"testing"

	"github.com/lemonsaurus/agency/internal/control"
)

func TestTaskLabelSpawnAndRestart(t *testing.T) {
	ctx := context.Background()
	mock := &testMock{}
	mgr := newTestManager(mock)
	if err := mgr.SpawnAgent(ctx, testController, control.RoleManager, "claude", "/tmp/project", "Cloud Harness Setup"); err != nil {
		t.Fatal(err)
	}
	if got := mgr.ListPanes()[0].TaskLabel; got != "Cloud Harness Setup" {
		t.Fatalf("task label = %q", got)
	}
	foundFolder, foundTask := false, false
	for _, call := range mock.findCalls("set-option") {
		if call[4] == "@agency_label" && call[5] == "project" {
			foundFolder = true
		}
		if call[4] == "@agency_task_label" && call[5] == "Cloud Harness Setup" {
			foundTask = true
		}
	}
	if !foundFolder || !foundTask {
		t.Fatalf("folder=%v, task=%v", foundFolder, foundTask)
	}
	mock.listOutput = "0\tcontrol\t%0\t0\tpi\t/tmp\t0\t100\tcontroller\t\t%0\t\t\t@0\t\t\n" +
		"1\tproject\t%1\t0\tclaude\t/tmp/project\t1\t200\tmanager\t%0\t%0\t\t\t@1\t\tCloud Harness Setup"
	restarted := newTestManager(mock)
	if err := restarted.AdoptOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	requester := control.Requester{PaneID: "%1", Role: control.RoleManager, RootID: "%0"}
	if got, err := restarted.TaskLabel(ctx, requester, "", nil); err != nil || got != "Cloud Harness Setup" {
		t.Fatalf("restored task label = %q, %v", got, err)
	}
	id, err := restarted.ReplacePane(ctx, requester, "handoff-pi", "/tmp/successor")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := restarted.TaskLabel(ctx, requester, id, nil); err != nil || got != "Cloud Harness Setup" {
		t.Fatalf("replacement task label = %q, %v", got, err)
	}
	for _, call := range mock.findCalls("set-option") {
		if call[3] == id && call[4] == "@agency_label" && call[5] == "successor" {
			return
		}
	}
	t.Fatal("replacement border did not use successor folder")
}

func TestTaskLabelEditing(t *testing.T) {
	ctx := context.Background()
	mock := &testMock{listOutput: "0\tmain\t%1\t0\tpi\t/tmp\t1\t200\tworker\t%0\t%0\t\t\t@0\t\tOriginal"}
	mgr := newTestManager(mock)
	if err := mgr.AdoptOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	worker := control.Requester{PaneID: "%1", Role: control.RoleWorker, RootID: "%0"}
	label := "error: \"quoted\" #{pane_id} 雲"
	if got, err := mgr.TaskLabel(ctx, worker, "", &label); err != nil || got != label {
		t.Fatalf("self edit = %q, %v", got, err)
	}
	other := control.Requester{PaneID: "%2", Role: control.RoleWorker, RootID: "%0"}
	if got, err := mgr.TaskLabel(ctx, other, "%1", nil); err != nil || got != label {
		t.Fatalf("read another pane = %q, %v", got, err)
	}
	if _, err := mgr.TaskLabel(ctx, other, "%1", &label); err == nil {
		t.Fatal("worker edited another pane")
	}
	invalid := "bad\nlabel"
	if _, err := mgr.TaskLabel(ctx, worker, "", &invalid); err == nil {
		t.Fatal("accepted multiline label")
	}
	mock.failTaskLabel = true
	rejected := "Not persisted"
	if _, err := mgr.TaskLabel(ctx, worker, "", &rejected); err == nil {
		t.Fatal("ignored persistence failure")
	}
	if got, _ := mgr.TaskLabel(ctx, worker, "", nil); got != label {
		t.Fatalf("failed write changed memory to %q", got)
	}
	mock.failTaskLabel = false
	for _, role := range []control.Role{control.RoleManager, control.RoleController} {
		label = string(role)
		if _, err := mgr.TaskLabel(ctx, control.Requester{PaneID: "%0", Role: role}, "%1", &label); err != nil {
			t.Fatal(err)
		}
	}
	label = ""
	if got, err := mgr.TaskLabel(ctx, worker, "", &label); err != nil || got != "" {
		t.Fatalf("clear = %q, %v", got, err)
	}
	if _, err := mgr.TaskLabel(ctx, worker, "%99", nil); err == nil {
		t.Fatal("accepted unknown pane")
	}
	if _, err := mgr.TaskLabel(ctx, control.Requester{Human: true}, "", nil); err == nil {
		t.Fatal("read a current pane without requester identity")
	}
}

func TestSpawnRejectsMissingAndInvalidTaskLabels(t *testing.T) {
	for _, label := range []string{"", "  ", "bad\nlabel", strings.Repeat("界", 101)} {
		mock := &testMock{}
		mgr := newTestManager(mock)
		if err := mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "/tmp", label); err == nil {
			t.Fatalf("accepted label %q", label)
		}
		if mock.findCall("split-window") != nil || mgr.PaneCount() != 0 {
			t.Fatal("created a pane for invalid label")
		}
	}
}

func TestTaskLabelPersistenceFailureRemovesNewPane(t *testing.T) {
	mock := &testMock{failTaskLabel: true}
	mgr := newTestManager(mock)
	if err := mgr.SpawnCommand(context.Background(), testController, control.RoleManager, "pi", "/tmp", "Task"); err == nil {
		t.Fatal("ignored label persistence failure")
	}
	if mgr.PaneCount() != 0 || mock.findCall("kill-pane") == nil {
		t.Fatal("retained a partially created pane")
	}
}
