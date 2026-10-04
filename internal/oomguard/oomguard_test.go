package oomguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallSetsOOMPolicyContinueOnPaneScopesAndTheDaemon(t *testing.T) {
	dir := t.TempDir()
	reloads := 0
	reload := func() error { reloads++; return nil }

	written, err := Install(dir, reload)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 || reloads != 1 {
		t.Fatalf("written = %v, reloads = %d", written, reloads)
	}
	for unit, section := range map[string]string{"tmux-spawn-.scope.d": "[Scope]", "agency.service.d": "[Service]"} {
		data, err := os.ReadFile(filepath.Join(dir, "systemd", "user", unit, "50-agency-oom.conf"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), section+"\nOOMPolicy=continue\n") {
			t.Fatalf("%s drop-in = %q", unit, data)
		}
	}

	written, err = Install(dir, reload)
	if err != nil || len(written) != 0 || reloads != 1 {
		t.Fatalf("second install: written = %v, err = %v, reloads = %d", written, err, reloads)
	}
}
