package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/manifest"
	"github.com/lemonsaurus/agency/internal/tmuxtest"
)

// When the tmux server dies the daemon starts a new one and brings every
// agent pane back with its role, parent, label and window, and a second run of
// restore finds nothing left to do.
func TestServeRestoresTheSessionAfterTmuxDies(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	home := t.TempDir()
	session := fmt.Sprintf("recover-test-%d", os.Getpid())
	os.MkdirAll(filepath.Join(home, ".config", "agency"), 0o700)
	os.WriteFile(filepath.Join(home, ".config", "agency", "config.toml"), []byte("[session]\nname = \""+session+"\"\n\n[admission]\nmin_available_gb = 0.1\nmax_load_per_core = 1000\nmax_memory_some60 = 100\nmax_memory_full10 = 100\nmemory_share = 1\ncpu_share = 1\nreserve_gb = 0.1\nreserve_cores = 0.1\n\n[agents.sh]\ncommand = \"sh\"\n"), 0o600)
	env := []string{"AGENCY_TEST_MAIN=1", "HOME=" + home, "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	socket := "agency-" + session
	tmux := func(args ...string) string {
		out, _ := exec.Command("tmux", append([]string{"-L", socket}, args...)...).CombinedOutput()
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
	daemon := exec.Command(os.Args[0], "serve")
	daemon.Env, daemon.Dir = env, home
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		daemon.Process.Kill()
		daemon.Wait()
		tmuxtest.Kill(socket)
		for _, suffix := range []string{".sock", ".lock", ".log"} {
			os.Remove(fmt.Sprintf("/tmp/agency-%s%s", session, suffix))
		}
	})
	var boss string
	wait := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(120 * time.Second); !done(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				log, _ := os.ReadFile(fmt.Sprintf("/tmp/agency-%s.log", session))
				data, _ := os.ReadFile(filepath.Join(home, ".local", "state", "agency", "manifest-"+session+".json"))
				t.Fatalf("timed out waiting for %s\nmanifest: %s\nlog: %s\npanes: %s", what, data, log, tmux("list-panes", "-a", "-F", "#{pane_id} #{session_name} #{@agency_role} #{@agency_task_label}")+"\nboss screen: "+tmux("capture-pane", "-p", "-t", boss))
			}
		}
	}
	wait("daemon", func() bool {
		cmd := exec.Command(os.Args[0], "capabilities")
		cmd.Env, cmd.Dir = env, home
		return cmd.Run() == nil
	})

	boss = agency("spawn", "--window", "alpha", "--label", "Boss", "sh")
	tmux("send-keys", "-t", boss, strings.Join(append([]string{"HOME=" + home, "AGENCY_TEST_MAIN=1", os.Args[0]}, "spawn", "--role", "manager", "--window", "beta", "--label", "Kid", "--cmd", "'sleep 600'"), " "), "Enter")
	saved := filepath.Join(home, ".local", "state", "agency", "manifest-"+session+".json")
	wait("manifest with two agents", func() bool {
		m, _ := manifest.Load(saved)
		return len(m.Panes()) >= 3 // control shell, Boss, Kid
	})

	tmuxtest.Kill(socket)

	pending := filepath.Join(home, ".local", "state", "agency", "restore-"+session+".json")
	panes := func() map[string][]string {
		out := map[string][]string{}
		for _, line := range strings.Split(tmux("list-panes", "-a", "-F", "#{pane_id}\t#{@agency_task_label}\t#{@agency_role}\t#{@agency_parent}\t#{@agency_restored}"), "\n") {
			fields := strings.Split(line, "\t")
			if len(fields) == 5 && fields[1] != "" {
				out[fields[1]] = fields
			}
		}
		return out
	}
	wait("both agents restored", func() bool {
		got := panes()
		_, gone := os.Stat(pending)
		return got["Boss"] != nil && got["Kid"] != nil && gone != nil
	})
	got := panes()
	if got["Boss"][2] != "controller" || got["Kid"][2] != "manager" {
		t.Fatalf("roles = %v", got)
	}
	if got["Kid"][3] != got["Boss"][0] {
		t.Fatalf("Kid's parent is %s, want Boss %s", got["Kid"][3], got["Boss"][0])
	}
	if got["Boss"][4] == "" || got["Kid"][4] == "" {
		t.Fatalf("restored panes do not name their old IDs: %v", got)
	}
	if out := agency("restore"); !strings.Contains(out, "Nothing to restore") {
		t.Fatalf("second restore = %q", out)
	}
	if out := agency("restore", "--json"); out != `{"type":"nothing"}` {
		t.Fatalf("restore --json = %q", out)
	}
	data, _ := os.ReadFile(saved)
	var m manifest.Manifest
	if json.Unmarshal(data, &m) != nil || len(m.Panes()) < 3 {
		t.Fatalf("manifest after restore = %s", data)
	}
}
