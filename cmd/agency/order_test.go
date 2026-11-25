package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/order"
)

// The sky box keeps one window order for every device: moves by group,
// index, or window id, concurrent moves from two devices, renames, and new
// and closed windows all land in the saved order, the box's tmux follows it
// in every grouped view session, and a daemon restart keeps it.
func TestServeKeepsTheWindowOrderAcrossMovesAndRestarts(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	home := t.TempDir()
	session := fmt.Sprintf("order-test-%d", os.Getpid())
	os.MkdirAll(filepath.Join(home, ".config", "agency"), 0o700)
	os.WriteFile(filepath.Join(home, ".config", "agency", "config.toml"), []byte("[session]\nname = \""+session+"\"\n\n[agents.sh]\ncommand = \"sh\"\n"), 0o600)
	// Without a UTF-8 locale tmux prints the tabs in its formats as underscores.
	env := []string{"AGENCY_TEST_MAIN=1", "HOME=" + home, "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	tmux := func(args ...string) string {
		out, err := exec.Command("tmux", append([]string{"-L", "agency-" + session}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	agency := func(args ...string) string {
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env, cmd.Dir = env, home
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("agency %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	var daemon *exec.Cmd
	start := func() {
		daemon = exec.Command(os.Args[0], "serve")
		daemon.Env, daemon.Dir = env, home
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			cmd := exec.Command(os.Args[0], "capabilities")
			cmd.Env, cmd.Dir = env, home
			if cmd.Run() == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("daemon did not start")
			}
		}
	}
	stop := func() {
		daemon.Process.Signal(syscall.SIGTERM)
		daemon.Wait()
	}
	t.Cleanup(func() {
		stop()
		exec.Command("tmux", "-L", "agency-"+session, "kill-server").Run()
		for _, suffix := range []string{".sock", ".lock", ".log"} {
			os.Remove(fmt.Sprintf("/tmp/agency-%s%s", session, suffix))
		}
	})
	start()
	open := func(group string) {
		pane := tmux("new-window", "-d", "-t", session+":", "-P", "-F", "#{pane_id}", "sleep 600")
		tmux("set-option", "-p", "-t", pane, "@agency_group", group)
	}
	// The daemon's first window is the default group. journalia has two
	// agents, so two windows.
	for _, group := range []string{"journalia", "agency", "journalia", "control"} {
		open(group)
	}
	tmux("new-session", "-d", "-t", session, "-s", "view-1")
	tmux("new-session", "-d", "-s", "park-1", "sleep 600")
	groups := func(s string) []string {
		var out []string
		for _, line := range strings.Split(tmux("list-windows", "-t", "="+s, "-F", "#{window_id}"), "\n") {
			group := tmux("display-message", "-p", "-t", line, "#{@agency_group}")
			if group == "" {
				group = "main"
			}
			if len(out) == 0 || out[len(out)-1] != group {
				out = append(out, group)
			}
		}
		return out
	}
	want := func(names ...string) {
		t.Helper()
		var got []string
		if err := json.Unmarshal([]byte(agency("window-order")), &got); err != nil || !slices.Equal(got, names) {
			t.Fatalf("window order = %v, %v; want %v", got, err, names)
		}
		for _, s := range []string{session, "view-1"} {
			if got := groups(s); !slices.Equal(got, names) {
				t.Fatalf("%s windows = %v, want %v", s, got, names)
			}
		}
		if n := tmux("display-message", "-p", "-t", "=park-1:", "#{session_windows}"); n != "1" {
			t.Fatalf("park-1 holds %s windows", n)
		}
	}
	want("main", "journalia", "agency", "control")

	agency("move-window", "control", "1")
	want("control", "main", "journalia", "agency")
	agency("move-window", "4", "2")
	want("control", "agency", "main", "journalia")
	journalia := strings.Fields(tmux("list-windows", "-t", session, "-f", "#{==:#{@agency_group},journalia}", "-F", "#{window_id}"))[0]
	agency("move-window", journalia, "1")
	want("journalia", "control", "agency", "main")

	// Two devices move different windows from the same stale order.
	base := []string{"journalia", "control", "agency", "main"}
	var wg sync.WaitGroup
	for _, local := range [][]string{
		{"main", "journalia", "control", "agency"},
		{"journalia", "agency", "control", "main"},
	} {
		moves, _ := json.Marshal(order.Moves(base, local))
		wg.Add(1)
		go func() {
			defer wg.Done()
			agency("window-order", string(moves))
		}()
	}
	wg.Wait()
	want("main", "journalia", "agency", "control")

	agency("rename-window", "journalia", "scratch")
	want("main", "scratch", "agency", "control")

	stop()
	start()
	want("main", "scratch", "agency", "control")

	open("fresh")
	tmux("kill-window", "-t", strings.Fields(tmux("list-windows", "-t", session, "-f", "#{==:#{@agency_group},agency}", "-F", "#{window_id}"))[0])
	want("main", "scratch", "control", "fresh")
	if saved, err := order.Read(filepath.Join(home, ".local", "state", "agency", "window-order.json")); err != nil || !slices.Equal(saved, []string{"main", "scratch", "control", "fresh"}) {
		t.Fatalf("saved order = %v, %v", saved, err)
	}
}
