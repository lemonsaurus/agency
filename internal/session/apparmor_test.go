package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/tmuxtest"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "apparmor")
	if err != nil {
		panic(err)
	}
	apparmorProfiles = dir
	tmuxtest.Sweep()
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func loadFakeProfile(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(apparmorProfiles, name+".7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "name"), []byte(name+"\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
}

func TestRoleCommandRunsUnderLoadedPaneProfile(t *testing.T) {
	loadFakeProfile(t, paneProfile)
	bin := t.TempDir()
	stubs := map[string]string{
		"aa-exec": "#!/bin/sh\n[ \"$1 $2 $3\" = \"-p agency-pane --\" ] || exit 9\nshift 3\nexec \"$@\"\n",
	}
	for name, script := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	command := roleCommand(`GREETING="it's" sh -c 'echo "$GREETING $AGENCY_ROLE $AGENCY_PANE_ID $AGENCY_PARENT_ID"'`, control.RoleWorker, "%3", "%1")
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "SHELL=/bin/sh", "TMUX_PANE=%5", "AGENCY_PANE_ID=%9"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", command, err, out)
	}
	if got, want := string(out), "it's worker %5 %3\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRoleCommandSkipsMissingPaneProfile(t *testing.T) {
	loadFakeProfile(t, paneProfile+"-other")
	command := roleCommand("pi", control.RoleManager, "%0", "%0")
	if got, want := command, `AGENCY_ROLE=manager AGENCY_PANE_ID="$TMUX_PANE" AGENCY_PARENT_ID="%0" AGENCY_ROOT_ID="%0" pi`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
