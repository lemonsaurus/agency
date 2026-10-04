package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as agency when AGENCY_TEST_MAIN is set, so tests can run real subcommands.
func TestMain(m *testing.M) {
	if os.Getenv("AGENCY_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Every daemon start records itself in the death log and leaves the death hooks on the tmux server:
// on a fresh server, and on a running one that lacks them, as after an upgrade.
func TestServeRecordsStartAndInstallsDeathHooks(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	home := t.TempDir()
	session := fmt.Sprintf("serve-test-%d", os.Getpid())
	os.MkdirAll(filepath.Join(home, ".config", "agency"), 0o700)
	os.WriteFile(filepath.Join(home, ".config", "agency", "config.toml"), []byte("[session]\nname = \""+session+"\"\n\n[agents.sh]\ncommand = \"sh\"\n"), 0o600)
	tmux := func(args ...string) string {
		out, _ := exec.Command("tmux", append([]string{"-L", "agency-" + session}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		tmux("kill-server")
		for _, suffix := range []string{".sock", ".lock", ".log"} {
			os.Remove(fmt.Sprintf("/tmp/agency-%s%s", session, suffix))
		}
	})
	deathLog := filepath.Join(home, ".agents", "run", "agency", "pane-deaths.jsonl")
	for start := 1; start <= 2; start++ {
		if start == 2 {
			tmux("set-hook", "-gu", "pane-died")
			tmux("set-hook", "-gu", "pane-exited")
			tmux("set", "-gu", "remain-on-exit")
		}
		daemon := exec.Command(os.Args[0], "serve")
		daemon.Env = []string{"AGENCY_TEST_MAIN=1", "HOME=" + home, "PATH=" + os.Getenv("PATH")}
		daemon.Dir = home
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			data, _ := os.ReadFile(deathLog)
			if strings.Count(string(data), `"kind":"daemon"`) == start {
				break
			}
			if time.Now().After(deadline) {
				daemon.Process.Kill()
				t.Fatalf("start %d: death log has no daemon entry: %q", start, data)
			}
			time.Sleep(50 * time.Millisecond)
		}
		for !strings.Contains(tmux("show-hooks", "-g", "pane-exited"), "pane-exit exited") && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		daemon.Process.Signal(syscall.SIGTERM)
		daemon.Wait()
		if hook := tmux("show-hooks", "-g", "pane-died"); !strings.Contains(hook, os.Args[0]+" pane-exit died") {
			t.Fatalf("start %d: pane-died hook = %q", start, hook)
		}
		if hook := tmux("show-hooks", "-g", "pane-exited"); !strings.Contains(hook, "pane-exit exited") {
			t.Fatalf("start %d: pane-exited hook = %q", start, hook)
		}
		if remain := tmux("show", "-gv", "remain-on-exit"); remain != "failed" {
			t.Fatalf("start %d: remain-on-exit = %q", start, remain)
		}
	}
}
