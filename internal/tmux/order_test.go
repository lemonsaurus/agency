package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/config"
)

// realTmux starts a private tmux server for the test and returns a client
// for session on it.
func realTmux(t *testing.T, session string) *Client {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	socket := fmt.Sprintf("agency-order-test-%d-%s", os.Getpid(), strings.ReplaceAll(t.Name(), "/", "-"))
	c := &Client{Cmd: &ExecCommander{SocketName: socket}, SessionName: session}
	t.Cleanup(func() { c.Cmd.Run(context.Background(), "kill-server") })
	return c
}

func (c *Client) windowNames(t *testing.T, session string) []string {
	t.Helper()
	out, err := c.Cmd.Run(context.Background(), "list-windows", "-t", "="+session, "-F", "#{window_name}")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(out, "\n")
}

// The box's windows sit in sessions grouped with the main one, and a parked
// viewer sits on a session of its own. Arranging the main session moves the
// whole group without gaps, leaves the park session alone, and keeps every
// grouped session on its window: a view session on its own window, even one
// it drifted from, and any other on the window it showed.
func TestArrangeWindowsMovesGroupedViewSessionsAndLeavesParkAlone(t *testing.T) {
	ctx := context.Background()
	c := realTmux(t, "cloud")
	run := func(args ...string) string {
		out, err := c.Cmd.Run(ctx, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	run("new-session", "-d", "-s", "cloud", "-n", "journalia", "sleep 600")
	for _, name := range []string{"agency", "control", "runtimes", "secretary"} {
		run("new-window", "-d", "-t", "cloud:", "-n", name, "sleep 600")
	}
	run("new-session", "-d", "-t", "cloud", "-s", "view-1")
	run("new-session", "-d", "-t", "cloud", "-s", "view-2")
	run("new-session", "-d", "-s", "park-1", "-n", "parked", "sleep 600")
	ids := map[string]string{}
	for _, line := range strings.Split(run("list-windows", "-t", "cloud", "-F", "#{window_name} #{window_id}"), "\n") {
		name, id, _ := strings.Cut(line, " ")
		ids[name] = id
	}
	run("select-window", "-t", "view-1:"+ids["runtimes"])
	run("set-option", "-t", "view-2", "@agency_window", ids["agency"])
	shows := map[string]string{"cloud": ids["journalia"], "view-1": ids["runtimes"], "view-2": ids["agency"]}
	for _, want := range [][]string{
		{"control", "journalia", "agency", "runtimes", "secretary"},
		{"secretary", "runtimes", "agency", "journalia", "control"},
		{"control", "secretary", "journalia", "agency", "runtimes"},
	} {
		var target []string
		for _, name := range want {
			target = append(target, ids[name])
		}
		if _, err := c.ArrangeWindows(ctx, target); err != nil {
			t.Fatal(err)
		}
		for _, session := range []string{"cloud", "view-1", "view-2"} {
			if got := c.windowNames(t, session); !slices.Equal(got, want) {
				t.Fatalf("%s = %v, want %v", session, got, want)
			}
			if got := run("display-message", "-p", "-t", "="+session+":", "#{window_id}"); got != shows[session] {
				t.Fatalf("%s shows %s, want %s", session, got, shows[session])
			}
		}
		if got := run("list-windows", "-t", "cloud", "-F", "#{window_index}"); got != "0\n1\n2\n3\n4" {
			t.Fatalf("cloud indexes = %q", got)
		}
		if got := c.windowNames(t, "park-1"); !slices.Equal(got, []string{"parked"}) {
			t.Fatalf("park-1 = %v", got)
		}
	}
	if made, err := c.ArrangeWindows(ctx, nil); err != nil || made != 0 {
		t.Fatalf("arranging an arranged session made %d moves, %v", made, err)
	}
}

// A device tells its daemon about every move of a sky window: a tmux move
// links the window again, and the menu's swaps report themselves. Moves in
// the local world stay quiet.
func TestDeviceConfigReportsSkyWindowMoves(t *testing.T) {
	ctx := context.Background()
	c := realTmux(t, "agency")
	dir := t.TempDir()
	reports := filepath.Join(dir, "reports")
	bin := filepath.Join(dir, "agency")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$*\" >> "+reports+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Cloud.Host = "sky"
	conf := buildTmuxConf(cfg, bin)
	if !strings.Contains(conf, `{ swap-window -t :-1 ; run-shell -b "`+bin+` window-moved" }`) {
		t.Fatal("the menu's swap does not report itself")
	}
	path := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := cfg.Session.RemoteName()
	for _, args := range [][]string{
		{"-f", path, "new-session", "-d", "-s", "agency", "sleep 600"},
		{"new-window", "-d", "-t", "agency:", "sleep 600"},
		{"new-session", "-d", "-s", remote, "sleep 600"},
		{"new-window", "-d", "-t", remote + ":", "sleep 600"},
		{"new-window", "-d", "-t", remote + ":", "sleep 600"},
	} {
		if _, err := c.Cmd.Run(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	reported := func() int {
		time.Sleep(300 * time.Millisecond)
		data, _ := os.ReadFile(reports)
		os.Remove(reports)
		return strings.Count(string(data), "window-moved")
	}
	reported()
	if _, err := c.Cmd.Run(ctx, "move-window", "-b", "-s", "agency:2", "-t", "agency:1"); err != nil {
		t.Fatal(err)
	}
	if n := reported(); n != 0 {
		t.Fatalf("a local move reported %d times", n)
	}
	if _, err := c.Cmd.Run(ctx, "move-window", "-b", "-s", remote+":3", "-t", remote+":1"); err != nil {
		t.Fatal(err)
	}
	if n := reported(); n == 0 {
		t.Fatal("a sky move went unreported")
	}
}
