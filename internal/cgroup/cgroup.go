// Package cgroup keeps panes and heavy jobs from starving the harness.
//
// The daemon and the tmux server sit in app.slice with CPUWeight 1000. Every
// pane gets its own scope in agency-panes.slice and every `agency job` its
// own scope in agency-jobs.slice, both under agency.slice, whose MemoryHigh
// leaves the harness room before the kernel has to reclaim from it:
//
//	app.slice/agency.service          the daemon       CPUWeight 1000
//	app.slice/agency-tmux.scope       the tmux server  CPUWeight 1000
//	agency.slice                      MemoryHigh = RAM - reserve
//	├─ agency-panes.slice             CPUWeight 100
//	│  └─ agency-pane-<id>.scope      CPUWeight by role
//	└─ agency-jobs.slice              CPUWeight 30
//	   └─ agency-job-<ticket>.scope
//
// Scopes adopt running processes, so moving the panes of a live server
// needs no restart.
package cgroup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lemonsaurus/agency/internal/control"
)

// Unit names.
const (
	DaemonUnit = "agency.service"
	TmuxUnit   = "agency-tmux.scope"
	RootSlice  = "agency.slice"
	PaneSlice  = "agency-panes.slice"
	JobSlice   = "agency-jobs.slice"
)

// HarnessWeight is the daemon's and tmux server's CPUWeight; the default is 100.
const HarnessWeight = 1000

// MemoryReserve is what agency.slice's MemoryHigh leaves for the harness and
// everything outside agency.
const MemoryReserve = 8 << 30

// PaneOOMScore is the oom_score_adj panes get. The user manager starts
// everything at 200 and only root may lower it, so panes are raised instead:
// the kernel's OOM killer then takes a build before tmux or the daemon.
const PaneOOMScore = 500

// PaneWeight is a pane scope's CPUWeight by role, so a controller stays
// responsive while its workers compile.
func PaneWeight(role control.Role) uint64 {
	switch role {
	case control.RoleController:
		return 400
	case control.RoleManager:
		return 200
	}
	return 100
}

// PaneUnit names the scope of pane %N.
func PaneUnit(paneID string) string {
	return "agency-pane-" + strings.TrimPrefix(paneID, "%") + ".scope"
}

// JobUnit names the scope of a job ticket.
func JobUnit(ticket string) string {
	return "agency-job-" + ticket + ".scope"
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

// Slices returns the slice unit files for a box with memTotal bytes of RAM,
// keyed by file name.
func Slices(memTotal uint64) map[string]string {
	high := "infinity"
	if memTotal > 2*MemoryReserve {
		high = strconv.FormatUint(memTotal-MemoryReserve, 10)
	}
	header := "# Written by agency: panes and jobs share this slice, so they cannot starve the daemon or tmux.\n"
	return map[string]string{
		RootSlice: header + "[Slice]\nMemoryHigh=" + high + "\n",
		PaneSlice: header + "[Slice]\nCPUWeight=100\n",
		JobSlice:  header + "[Slice]\nCPUWeight=30\n",
	}
}

// Install writes the slice units under configDir/systemd/user and returns
// the paths it changed. The caller reloads the user manager when any did.
func Install(configDir string, memTotal uint64) ([]string, error) {
	dir := filepath.Join(configDir, "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var written []string
	for name, content := range Slices(memTotal) {
		path := filepath.Join(dir, name)
		if current, err := os.ReadFile(path); err == nil && string(current) == content {
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return written, err
		}
		written = append(written, path)
	}
	return written, nil
}

// Reload makes the user manager read changed unit files.
func Reload(ctx context.Context) error {
	return run(ctx, "systemctl", "--user", "daemon-reload")
}

// Weigh sets a running unit's CPUWeight until the user manager restarts.
func Weigh(ctx context.Context, unit string, weight uint64) error {
	return run(ctx, "systemctl", "--user", "set-property", "--runtime", unit, "CPUWeight="+strconv.FormatUint(weight, 10))
}

// Adopt moves pids into a new transient scope in slice. The scope goes away
// once its last process exits.
func Adopt(ctx context.Context, unit, slice string, weight uint64, pids []int) error {
	if len(pids) == 0 {
		return nil
	}
	args := []string{
		"--user", "--quiet", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
		unit, "fail", "4", "PIDs", "au", strconv.Itoa(len(pids)),
	}
	for _, pid := range pids {
		args = append(args, strconv.Itoa(pid))
	}
	if slice == "" {
		slice = "app.slice"
	}
	args = append(args, "Slice", "s", slice, "CPUWeight", "t", strconv.FormatUint(weight, 10),
		"CollectMode", "s", "inactive-or-failed", "0")
	return run(ctx, "busctl", args...)
}

// Attach moves pids into a scope that already runs.
func Attach(ctx context.Context, unit string, pids []int) error {
	args := []string{
		"--user", "--quiet", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager", "AttachProcessesToUnit", "ssau", unit, "", strconv.Itoa(len(pids)),
	}
	for _, pid := range pids {
		args = append(args, strconv.Itoa(pid))
	}
	return run(ctx, "busctl", args...)
}

// Place moves pids into unit, starting the scope or joining it when it
// already runs.
func Place(ctx context.Context, unit, slice string, weight uint64, pids []int) error {
	err := Adopt(ctx, unit, slice, weight, pids)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return Attach(ctx, unit, pids)
	}
	return err
}

// Unit returns the systemd unit holding pid, such as agency-pane-12.scope.
func Unit(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Base(path)
		}
	}
	return ""
}

// RaiseOOMScore sets pid's oom_score_adj to at least score.
func RaiseOOMScore(pid, score int) error {
	path := "/proc/" + strconv.Itoa(pid) + "/oom_score_adj"
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if current, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && current >= score {
		return nil
	}
	return os.WriteFile(path, []byte(strconv.Itoa(score)), 0o644)
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
