package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/order"
	"github.com/lemonsaurus/agency/internal/tmux"
	"github.com/lemonsaurus/agency/internal/tmuxtest"
)

// skyBox stands in for the box over SSH: it lists remote windows and keeps
// the window order the way the box daemon does, for any number of devices.
type skyBox struct {
	mu      sync.Mutex
	windows []cloud.Window
	order   []string
	sent    [][]order.Move
}

func (b *skyBox) Windows(context.Context) ([]cloud.Window, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.windows), nil
}

func (b *skyBox) Run(_ context.Context, _ time.Duration, args ...string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if args[0] != "window-order" {
		return "", fmt.Errorf("unexpected %v", args)
	}
	var moves []order.Move
	if err := json.Unmarshal([]byte(args[1]), &moves); err != nil {
		return "", err
	}
	if len(moves) > 0 {
		b.sent = append(b.sent, moves)
	}
	b.order = order.Apply(b.order, moves)
	out, _ := json.Marshal(b.order)
	return string(out), nil
}

func (b *skyBox) orderNow() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.order)
}

// skyDevice is a device with a real tmux server whose sky world holds one
// window per group, opened in the order given, each with idle viewer panes.
type skyDevice struct {
	t    *testing.T
	mgr  *Manager
	tmux *tmux.Client
}

func newSkyDevice(t *testing.T, box *skyBox, opened ...string) *skyDevice {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	socket := fmt.Sprintf("agency-sky-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	tc := &tmux.Client{Cmd: &tmux.ExecCommander{SocketName: socket}, SessionName: "test"}
	t.Cleanup(func() { tmuxtest.Kill(socket) })
	mgr := newTestManager(&testMock{})
	mgr.tmux = tc
	mgr.remote = &tmux.Client{Cmd: tc.Cmd, SessionName: mgr.cfg.Session.RemoteName()}
	mgr.cfg.Cloud.Host = "sky"
	mgr.cloud = box
	d := &skyDevice{t: t, mgr: mgr, tmux: tc}
	conf := filepath.Join(t.TempDir(), "tmux.conf")
	os.WriteFile(conf, []byte("set -g base-index 1\nset -g renumber-windows on\n"), 0o644)
	d.run("-f", conf, "new-session", "-d", "-s", "test", "sleep 600")
	remote := mgr.remote.SessionName
	for i, group := range opened {
		if i == 0 {
			d.run("new-session", "-d", "-s", remote, "-n", group, "sleep 600")
		} else {
			d.run("new-window", "-d", "-t", remote+":", "-n", group, "sleep 600")
		}
	}
	panes := d.run("list-panes", "-s", "-t", remote, "-F", "#{pane_id} #{window_name}")
	for _, line := range strings.Split(panes, "\n") {
		pane, group, _ := strings.Cut(line, " ")
		for _, window := range box.windows {
			if cloud.Group(window.Pane) != group {
				continue
			}
			if !strings.Contains(d.run("list-panes", "-a", "-F", "#{@agency_cloud}"), window.ID) {
				if d.run("show-options", "-pqv", "-t", pane, "@agency_cloud") != "" {
					pane = d.run("split-window", "-d", "-t", pane, "-P", "-F", "#{pane_id}", "sleep 600")
				}
				d.run("set-option", "-p", "-t", pane, "@agency_cloud", window.ID)
			}
		}
	}
	return d
}

func (d *skyDevice) run(args ...string) string {
	d.t.Helper()
	out, err := d.tmux.Cmd.Run(context.Background(), args...)
	if err != nil {
		d.t.Fatal(err)
	}
	return out
}

func (d *skyDevice) sync() {
	d.t.Helper()
	summary, err := d.mgr.SyncCloud(context.Background())
	if err != nil || strings.Contains(summary, "unavailable") {
		d.t.Fatalf("sync: %s, %v", summary, err)
	}
}

func (d *skyDevice) windows() []string {
	d.t.Helper()
	return strings.Split(d.run("list-windows", "-t", d.mgr.remote.SessionName, "-F", "#{window_name}"), "\n")
}

func (d *skyDevice) window(name string) string {
	d.t.Helper()
	for _, line := range strings.Split(d.run("list-windows", "-t", d.mgr.remote.SessionName, "-F", "#{window_id} #{window_name}"), "\n") {
		if id, n, _ := strings.Cut(line, " "); n == name {
			return id
		}
	}
	d.t.Fatalf("no window %s", name)
	return ""
}

func (d *skyDevice) want(want ...string) {
	d.t.Helper()
	if got := d.windows(); !slices.Equal(got, want) {
		d.t.Fatalf("sky windows = %v, want %v", got, want)
	}
}

func newSkyBox(order ...string) *skyBox {
	return &skyBox{order: order, windows: []cloud.Window{
		{ID: "@9", Pane: tmux.PaneInfo{ID: "%1", Group: "journalia"}},
		{ID: "@10", Pane: tmux.PaneInfo{ID: "%2", Group: "control"}},
		{ID: "@11", Pane: tmux.PaneInfo{ID: "%3", Group: "agency"}},
		{ID: "@12", Pane: tmux.PaneInfo{ID: "%4", Group: "journalia"}},
	}}
}

func TestResumedDeviceTakesTheBoxOrder(t *testing.T) {
	box := newSkyBox("control", "journalia", "agency")
	d := newSkyDevice(t, box, "agency", "journalia", "control")
	d.sync()
	d.want("control", "journalia", "agency")
	if len(box.sent) != 0 {
		t.Fatalf("a fresh device sent moves: %v", box.sent)
	}
	// The lease went elsewhere and came back; the windows reopen in whatever
	// order the viewers came up.
	d.mgr.skyBase = nil
	d.run("swap-window", "-d", "-s", d.window("control"), "-t", d.window("agency"))
	d.sync()
	d.want("control", "journalia", "agency")
	if len(box.sent) != 0 {
		t.Fatalf("a resumed device sent moves: %v", box.sent)
	}
}

func TestTmuxMoveOnADeviceMovesTheBoxOrder(t *testing.T) {
	box := newSkyBox("control", "journalia", "agency")
	d := newSkyDevice(t, box, "control", "journalia", "agency")
	d.sync()
	d.run("swap-window", "-d", "-s", d.window("control"), "-t", d.window("agency"))
	if err := d.mgr.WindowMoved(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := box.orderNow(); !slices.Equal(got, []string{"agency", "journalia", "control"}) {
		t.Fatalf("box order = %v", got)
	}
	d.want("agency", "journalia", "control")
	// A sync that beats the hook still carries the move.
	d.run("move-window", "-a", "-s", d.window("agency"), "-t", d.window("control"))
	d.sync()
	if got := box.orderNow(); !slices.Equal(got, []string{"journalia", "control", "agency"}) {
		t.Fatalf("box order after a sync = %v", got)
	}
	d.want("journalia", "control", "agency")
}

func TestAgencyMoveWindowOnADeviceMovesASkyWindow(t *testing.T) {
	box := newSkyBox("control", "journalia", "agency")
	d := newSkyDevice(t, box, "control", "journalia", "agency")
	d.sync()
	if err := d.mgr.MoveWindow(context.Background(), "agency", 1); err != nil {
		t.Fatal(err)
	}
	if got := box.orderNow(); !slices.Equal(got, []string{"agency", "control", "journalia"}) {
		t.Fatalf("box order = %v", got)
	}
	d.want("agency", "control", "journalia")
}

func TestMovesFromTwoDevicesBothLand(t *testing.T) {
	box := newSkyBox("journalia", "agency", "control")
	desktop := newSkyDevice(t, box, "journalia", "agency", "control")
	laptop := newSkyDevice(t, box, "journalia", "agency", "control")
	desktop.sync()
	laptop.sync()
	desktop.run("move-window", "-b", "-s", desktop.window("control"), "-t", desktop.window("journalia"))
	laptop.run("swap-window", "-d", "-s", laptop.window("journalia"), "-t", laptop.window("agency"))
	var wg sync.WaitGroup
	for _, d := range []*skyDevice{desktop, laptop} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.mgr.WindowMoved(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	want := []string{"control", "agency", "journalia"}
	if got := box.orderNow(); !slices.Equal(got, want) {
		t.Fatalf("box order = %v, want %v", got, want)
	}
	desktop.sync()
	laptop.sync()
	desktop.want(want...)
	laptop.want(want...)
}

func TestWindowRenamedInTmuxKeepsItsGroupsPlace(t *testing.T) {
	box := newSkyBox("journalia", "control", "agency")
	d := newSkyDevice(t, box, "journalia", "control", "agency")
	d.sync()
	d.run("rename-window", "-t", d.window("journalia"), "scratch")
	if err := d.mgr.WindowMoved(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(box.sent) != 0 {
		t.Fatalf("a rename sent moves: %v", box.sent)
	}
	d.sync()
	d.want("journalia", "control", "agency")
}

func TestSameNamedWindowsStayTogether(t *testing.T) {
	box := newSkyBox("control", "agency", "journalia")
	d := newSkyDevice(t, box, "journalia", "control", "agency")
	// A second window took the name control; its viewer shows journalia.
	d.run("rename-window", "-t", d.window("journalia"), "control")
	d.mgr.skyGroups = map[string]string{"@9": "journalia", "@12": "journalia", "@10": "control", "@11": "agency"}
	windows, err := d.mgr.skyWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := groupNames(windows); !slices.Equal(got, []string{"journalia", "control", "agency"}) {
		t.Fatalf("groups = %v", got)
	}
	d.sync()
	d.want("control", "agency", "journalia")
}

func TestCollapsedDeviceForgetsItsOrder(t *testing.T) {
	box := newSkyBox("control", "journalia", "agency")
	d := newSkyDevice(t, box, "control", "journalia", "agency")
	d.sync()
	d.mgr.collapseCloud(context.Background(), nil, []string{"%0"})
	if d.mgr.skyBase != nil {
		t.Fatal("a collapsed device kept the order its windows had")
	}
}
