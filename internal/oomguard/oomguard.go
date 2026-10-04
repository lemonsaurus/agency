// Package oomguard keeps systemd from killing a whole pane when the kernel
// OOM-kills one process inside it.
//
// tmux 3.4 starts every pane in its own transient tmux-spawn-<uuid>.scope.
// systemd's default OOMPolicy=stop then answers an OOM kill of any process in
// the scope, such as one tsc worker, by SIGKILLing the rest of the scope,
// including the Pi that ran the command. OOMPolicy=continue lets the kernel's
// victim die alone. agency.service gets the same policy: panes whose scope
// setup failed stay in the daemon's cgroup.
package oomguard

import (
	"os"
	"os/exec"
	"path/filepath"
)

const fileName = "50-agency-oom.conf"

var dropIns = []struct{ dir, section string }{
	{"tmux-spawn-.scope.d", "Scope"},
	{"agency.service.d", "Service"},
}

// Active reports whether a systemd user manager runs for this user.
func Active() bool {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(runtime, "systemd", "private"))
	return err == nil
}

// Install writes the drop-ins under configDir/systemd/user and calls reload
// when any changed, so running scopes pick the policy up too. It returns the
// paths it wrote.
func Install(configDir string, reload func() error) ([]string, error) {
	var written []string
	for _, dropIn := range dropIns {
		dir := filepath.Join(configDir, "systemd", "user", dropIn.dir)
		path := filepath.Join(dir, fileName)
		content := "# Written by agency: an OOM kill takes only the kernel's victim, not the whole pane.\n[" + dropIn.section + "]\nOOMPolicy=continue\n"
		if current, err := os.ReadFile(path); err == nil && string(current) == content {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return written, err
		}
		written = append(written, path)
	}
	if len(written) == 0 {
		return nil, nil
	}
	return written, reload()
}

// Reload is `systemctl --user daemon-reload`.
func Reload() error {
	return exec.Command("systemctl", "--user", "daemon-reload").Run()
}
