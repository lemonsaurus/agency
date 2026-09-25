package session

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/tmux"
)

func TestCloudViewerMirrorsFolderAndTaskLabel(t *testing.T) {
	ctx := context.Background()
	mock := &testMock{}
	mgr := newTestManager(mock)
	remote := tmux.PaneInfo{CWD: "/srv/cloud-project", TaskLabel: "Cloud Harness Setup"}
	id := mgr.spawnViewer(ctx, cloud.Window{ID: "@9", Name: "π pi@old-folder", Pane: remote}, "pi")
	if id == "" {
		t.Fatal("viewer not created")
	}
	if err := mgr.mirrorViewer(ctx, id, remote); err != nil {
		t.Fatal(err)
	}
	remote.CWD = "/srv/new-project"
	remote.TaskLabel = "Updated cloud task"
	if err := mgr.mirrorViewer(ctx, id, remote); err != nil {
		t.Fatal(err)
	}
	options := map[string]string{}
	for _, call := range mock.findCalls("set-option") {
		options[call[4]] = call[5]
	}
	if options["@agency_label"] != "new-project" || options["@agency_task_label"] != remote.TaskLabel {
		t.Fatalf("mirrored options = %v", options)
	}
	if mgr.ListPanes()[0].TaskLabel != remote.TaskLabel {
		t.Fatal("tracked label was not updated")
	}
	mock.listOutput = "1\tcloud-harness\t%1\t0\tssh\t/tmp/local-folder\t1\t200\t\t\t\t\t\t@1\t@9\tUpdated cloud task"
	restarted := newTestManager(mock)
	if err := restarted.AdoptOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if got := restarted.ListPanes()[0].TaskLabel; got != remote.TaskLabel {
		t.Fatalf("adopted viewer task label = %q", got)
	}
	label := "Local edit"
	if _, err := restarted.TaskLabel(ctx, control.Requester{Role: control.RoleController}, "%1", &label); err == nil {
		t.Fatal("edited a cloud pane without a configured host")
	}
	remote.TaskLabel = ""
	if err := restarted.mirrorViewer(ctx, "%1", remote); err != nil {
		t.Fatal(err)
	}
	if restarted.ListPanes()[0].TaskLabel != "" {
		t.Fatal("cleared remote label retained locally")
	}
}

type mockCloud struct {
	windows    []cloud.Window
	calls      [][]string
	windowsErr error
	runErr     error
}

func (c *mockCloud) Windows(context.Context) ([]cloud.Window, error) {
	return c.windows, c.windowsErr
}

func (c *mockCloud) Run(_ context.Context, _ time.Duration, args ...string) (string, error) {
	c.calls = append(c.calls, args)
	return "", c.runErr
}

func TestLabelCloudViewerRoutesToRemotePane(t *testing.T) {
	ctx := context.Background()
	mock := &testMock{listOutput: "1\tcloud-harness\t%1\t0\tssh\t/tmp/local-folder\t1\t200\t\t\t\t\t\t@1\t@9\tOriginal"}
	mgr := newTestManager(mock)
	mgr.cfg.Cloud.Host = "cloud"
	remote := &mockCloud{windows: []cloud.Window{{ID: "@9", Pane: tmux.PaneInfo{ID: "%42", CWD: "/srv/cloud-project", TaskLabel: "Original"}}}}
	mgr.cloud = remote
	if err := mgr.AdoptOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	label := "Cloud Harness Setup"
	worker := control.Requester{PaneID: "%2", Role: control.RoleWorker}
	if _, err := mgr.TaskLabel(ctx, worker, "%1", &label); err == nil || len(remote.calls) != 0 {
		t.Fatal("worker could route another pane's label write")
	}
	for _, role := range []control.Role{control.RoleManager, control.RoleController} {
		requester := control.Requester{PaneID: "%0", Role: role}
		if got, err := mgr.TaskLabel(ctx, requester, "%1", &label); err != nil || got != label {
			t.Fatalf("remote edit = %q, %v", got, err)
		}
		call := remote.calls[len(remote.calls)-1]
		if strings.Join(call, "|") != "label|--pane|%42|--|Cloud Harness Setup" {
			t.Fatalf("remote command = %v", call)
		}
		if got, err := mgr.TaskLabel(ctx, requester, "%1", nil); err != nil || got != label {
			t.Fatalf("mirrored task label = %q, %v", got, err)
		}
	}
	remote.runErr = fmt.Errorf("remote unavailable")
	rejected := "Not saved"
	if _, err := mgr.TaskLabel(ctx, testController, "%1", &rejected); err == nil {
		t.Fatal("ignored remote failure")
	}
	if got, _ := mgr.TaskLabel(ctx, testController, "%1", nil); got != label {
		t.Fatalf("remote failure changed local label = %q", got)
	}
	remote.runErr = nil
	label = ""
	if got, err := mgr.TaskLabel(ctx, testController, "%1", &label); err != nil || got != "" {
		t.Fatalf("remote clear = %q, %v", got, err)
	}
	remote.windows = nil
	if _, err := mgr.TaskLabel(ctx, testController, "%1", &label); err == nil {
		t.Fatal("accepted vanished cloud window")
	}
}

func TestSyncCloudPlacesViewersByRemoteGroup(t *testing.T) {
	ctx := context.Background()
	// %1 still sits in the old local cloud-harness window; @10 has no viewer yet.
	mock := &testMock{listOutput: "3\tcloud-harness\t%1\t0\tssh\t/tmp\t1\t200\t\t\t\t\t\t@1\t@9\t\t\tagency"}
	mgr := newTestManager(mock)
	mgr.cfg.Cloud.Host = "cloud"
	mgr.cloud = &mockCloud{windows: []cloud.Window{
		{ID: "@9", Pane: tmux.PaneInfo{ID: "%42", CWD: "/srv/journalia", Group: "journalia"}},
		{ID: "@10", Pane: tmux.PaneInfo{ID: "%43", CWD: "/srv/agency"}},
	}}
	if _, err := mgr.SyncCloud(ctx); err != nil {
		t.Fatal(err)
	}
	moved := mock.findCall("break-pane")
	if strings.Join(moved, " ") != "break-pane -d -s %1 -n journalia -t remote-agency:" {
		t.Fatalf("misplaced viewer move = %v", moved)
	}
	created := mock.findCall("new-window")
	if strings.Join(created[:5], " ") != "new-window -t remote-agency: -n "+cloud.DefaultGroup {
		t.Fatalf("new viewer window = %v", created)
	}
	if !strings.HasSuffix(created[len(created)-1], "cloud-view @10") {
		t.Fatalf("new viewer command = %v", created)
	}
}

func TestRenameRemoteWorldWindowRenamesRemoteGroup(t *testing.T) {
	ctx := context.Background()
	mock := &testMock{
		listOutput:   "1\tmain\t%1\t0\tssh\t/tmp\t1\t200\t\t\t\t\t\t@3\t@9\t\t\tremote-agency",
		windowOutput: "@3\t1\tmain",
	}
	mgr := newTestManager(mock)
	mgr.cfg.Cloud.Host = "cloud"
	remote := &mockCloud{}
	mgr.cloud = remote
	if err := mgr.RenameWindow(ctx, "@3", "journalia"); err != nil {
		t.Fatal(err)
	}
	if len(remote.calls) != 1 || strings.Join(remote.calls[0], "|") != "rename-window|main|journalia" {
		t.Fatalf("remote calls = %v", remote.calls)
	}
	if renamed := mock.findCall("rename-window"); strings.Join(renamed, " ") != "rename-window -t @3 journalia" {
		t.Fatalf("local rename = %v", renamed)
	}
}

func TestBoxRenameWindowRegroupsAgents(t *testing.T) {
	ctx := context.Background()
	mock := &testMock{listOutput: "1\tπ pi@a\t%1\t0\tpi\t/tmp\t1\t200\t\t\t\t\t\t@1\t\t\t\tcloud\n" +
		"2\tπ pi@b\t%2\t0\tpi\t/tmp\t1\t201\t\t\t\t\t\t@2\t\t\tphone\tcloud"}
	mgr := newTestManager(mock)
	mgr.WindowPerPane = true
	if err := mgr.RenameWindow(ctx, cloud.DefaultGroup, "journalia"); err != nil {
		t.Fatal(err)
	}
	calls := mock.findCalls("set-option")
	if len(calls) != 1 || strings.Join(calls[0], " ") != "set-option -p -t %1 @agency_group journalia" {
		t.Fatalf("regroup calls = %v", calls)
	}
	if err := mgr.RenameWindow(ctx, "missing", "x"); err == nil {
		t.Fatal("renamed a group with no agents")
	}
}

func TestBoxSpawnJoinsRequesterGroup(t *testing.T) {
	mock := &testMock{listOutput: "1\tπ pi@a\t%0\t0\tpi\t/tmp\t1\t200\t\t\t\t\t\t@1\t\t\tjournalia\tcloud"}
	mgr := newTestManager(mock)
	mgr.WindowPerPane = true
	if err := mgr.SpawnCommand(context.Background(), testController, control.RoleManager, "pi", "/tmp", "Task"); err != nil {
		t.Fatal(err)
	}
	for _, call := range mock.findCalls("set-option") {
		if strings.Join(call, " ") == "set-option -p -t %1 @agency_group journalia" {
			return
		}
	}
	t.Fatalf("set-option calls = %v", mock.findCalls("set-option"))
}

func TestBoxKillWindowKillsGroup(t *testing.T) {
	ctx := context.Background()
	mock := &testMock{listOutput: "1\tπ pi@a\t%1\t0\tpi\t/tmp\t1\t200\t\t\t\t\t\t@1\t\t\tjournalia\tcloud\n" +
		"2\tπ pi@b\t%2\t0\tpi\t/tmp\t1\t201\t\t\t\t\t\t@2\t\t\t\tcloud\n" +
		"1\tπ pi@a\t%1\t0\tpi\t/tmp\t1\t200\t\t\t\t\t\t@1\t\t\tjournalia\tview-7"}
	mgr := newTestManager(mock)
	mgr.WindowPerPane = true
	if err := mgr.KillWindow(ctx, "journalia"); err != nil {
		t.Fatal(err)
	}
	if calls := mock.findCalls("kill-pane"); len(calls) != 1 || calls[0][2] != "%1" {
		t.Fatalf("kill calls = %v", calls)
	}
}
