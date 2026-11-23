package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lemonsaurus/agency/internal/budget"
	"github.com/lemonsaurus/agency/internal/cgroup"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/ipc"
	"github.com/lemonsaurus/agency/internal/session"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// scopeInterval is how often the daemon moves new panes into their scopes.
const scopeInterval = 15 * time.Second

func budgetStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "agency", "budget")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "agency", "budget")
}

// admissionHandler puts the headless server's spawns through the budget. The
// returned attach wires the budget into the socket server once it exists.
func admissionHandler(ctx context.Context, cfg *config.Config, cloud bool, tc *tmux.Client, mgr *session.Manager) (ipc.Handler, func(*ipc.Server)) {
	if !cloud {
		return mgr, func(*ipc.Server) {}
	}
	b := budget.New(admissionLimits(cfg.Admission), budgetStateDir())
	b.Panes = func(ctx context.Context) []budget.Pane { return budgetPanes(ctx, tc) }
	admission := ipc.NewAdmission(ctx, mgr, b, budgetStateDir())
	return admission, func(srv *ipc.Server) {
		srv.Budget, srv.Admission = b, admission
		b.OnStart = func(s budget.Started) { admission.Started(s, srv.Notify) }
		admission.RestoreQueue()
		go b.Run(ctx)
		go keepScopes(ctx, tc)
		go watchAdmission(ctx, b)
	}
}

// configPoll is how often the daemon checks the config file for new limits.
const configPoll = 5 * time.Second

// admissionLimits applies the config's overrides to the default limits.
func admissionLimits(a config.AdmissionConfig) budget.Limits {
	l := budget.DefaultLimits
	set := func(dst *float64, v float64) {
		if v > 0 {
			*dst = v
		}
	}
	set(&l.CPUShare, a.CPUShare)
	set(&l.MemShare, a.MemoryShare)
	set(&l.Reserve.CPU, a.ReserveCores)
	set(&l.JobCost.CPU, a.JobCores)
	set(&l.MaxMemorySome60, a.MaxMemorySome60)
	set(&l.MaxMemoryFull10, a.MaxMemoryFull10)
	set(&l.MaxLoadPerCore, a.MaxLoadPerCore)
	if a.ReserveGB > 0 {
		l.Reserve.Mem = uint64(a.ReserveGB * budget.GB)
	}
	if a.JobGB > 0 {
		l.JobCost.Mem = uint64(a.JobGB * budget.GB)
	}
	if a.MinAvailableGB > 0 {
		l.MinAvailable = uint64(a.MinAvailableGB * budget.GB)
	}
	if d, err := time.ParseDuration(a.MaxWait); err == nil && d > 0 {
		l.MaxWait = d
	}
	return l
}

// watchAdmission rereads [admission] whenever the config file changes, so
// tuning the budget never needs a restart. A file that fails to parse keeps
// the limits in force.
func watchAdmission(ctx context.Context, b *budget.Budget) {
	path := config.Path()
	modified := func() time.Time {
		info, err := os.Stat(path)
		if err != nil {
			return time.Time{}
		}
		return info.ModTime()
	}
	last := modified()
	ticker := time.NewTicker(configPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if at := modified(); !at.Equal(last) {
			last = at
			cfg, err := config.LoadFromPath(path)
			if err != nil {
				log.Printf("budget: keeping limits, %v", err)
				continue
			}
			limits := admissionLimits(cfg.Admission)
			b.SetLimits(limits)
			log.Printf("budget: limits reloaded: cpu %.0f%%, memory %.0f%%, reserve %s, max wait %s",
				limits.CPUShare*100, limits.MemShare*100, limits.Reserve, limits.MaxWait)
		}
	}
}

// budgetPanes lists the main session's panes. Viewer sessions repeat every
// pane, so it lists one session, never -a.
func budgetPanes(ctx context.Context, tc *tmux.Client) []budget.Pane {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := tc.Cmd.Run(ctx, "list-panes", "-s", "-t", tc.SessionName, "-F",
		"#{pane_id}\t#{pane_pid}\t#{@agency_role}\t#{@agency_command}\t#{pane_current_command}\t#{@agency_task_label}\t#{@agency_group}\t#{pane_current_path}\t#{window_activity}")
	if err != nil {
		return nil
	}
	var panes []budget.Pane
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 9 {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		activity, _ := strconv.ParseInt(f[8], 10, 64)
		role := control.Role(f[2])
		panes = append(panes, budget.Pane{
			ID:       f[0],
			PID:      pid,
			Role:     role,
			Class:    paneClass(role, f[3], f[4]),
			Label:    f[5],
			Window:   f[6],
			CWD:      f[7],
			Activity: time.Unix(activity, 0),
		})
	}
	return panes
}

// paneClass names a pane's kind the way ipc.SpawnClass names a spawn's.
func paneClass(role control.Role, encodedCommand, current string) string {
	command := current
	if decoded, err := base64.RawStdEncoding.DecodeString(encodedCommand); err == nil && len(decoded) > 0 {
		command = string(decoded)
	}
	return ipc.SpawnClass(role, command)
}

// keepScopes lays out the slices once, then keeps the tmux server and every
// pane in its scope.
func keepScopes(ctx context.Context, tc *tmux.Client) {
	if !cgroup.Active() {
		return
	}
	if report, err := applyScopes(ctx, tc); err != nil {
		log.Printf("cgroup: %v", err)
	} else if report.moved() > 0 {
		log.Printf("cgroup: %s", report)
	}
	ticker := time.NewTicker(scopeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report := scopePanes(ctx, tc)
			if report.moved() > 0 {
				log.Printf("cgroup: %s", report)
			}
		}
	}
}

// scopeReport counts what one pass moved.
type scopeReport struct {
	Slices    []string
	Tmux      bool
	Panes     int
	Processes int
	Failed    []string
}

func (r scopeReport) moved() int {
	n := r.Panes + len(r.Slices)
	if r.Tmux {
		n++
	}
	return n
}

func (r scopeReport) String() string {
	s := fmt.Sprintf("%d panes (%d processes) moved into scopes", r.Panes, r.Processes)
	if r.Tmux {
		s += ", tmux server moved to " + cgroup.TmuxUnit
	}
	if len(r.Slices) > 0 {
		s += ", wrote " + strings.Join(r.Slices, " ")
	}
	if len(r.Failed) > 0 {
		s += "; failed: " + strings.Join(r.Failed, "; ")
	}
	return s
}

// applyScopes installs the slices, raises the daemon's and tmux server's CPU
// weight, and moves every pane. Running it again changes nothing.
func applyScopes(ctx context.Context, tc *tmux.Client) (scopeReport, error) {
	var report scopeReport
	config, err := os.UserConfigDir()
	if err != nil {
		return report, err
	}
	total, _ := memTotal()
	written, err := cgroup.Install(config, total)
	if err != nil {
		return report, err
	}
	report.Slices = written
	if len(written) > 0 {
		if err := cgroup.Reload(ctx); err != nil {
			return report, err
		}
	}
	if err := cgroup.Weigh(ctx, cgroup.DaemonUnit, cgroup.HarnessWeight); err != nil {
		report.Failed = append(report.Failed, err.Error())
	}
	pane := scopePanes(ctx, tc)
	pane.Slices = report.Slices
	pane.Failed = append(report.Failed, pane.Failed...)
	return pane, nil
}

// scopePanes moves the tmux server into its scope and each pane's processes
// into the pane's own. A process is moved only from where tmux started it,
// so the scopes of jobs and of tools that made their own stay as they are.
func scopePanes(ctx context.Context, tc *tmux.Client) scopeReport {
	var report scopeReport
	if out, err := tc.Cmd.Run(ctx, "display-message", "-p", "#{pid}"); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && cgroup.Unit(pid) != cgroup.TmuxUnit {
			if err := cgroup.Place(ctx, cgroup.TmuxUnit, "app.slice", cgroup.HarnessWeight, []int{pid}); err != nil {
				report.Failed = append(report.Failed, err.Error())
			} else {
				report.Tmux = true
			}
		}
	}
	panes := budgetPanes(ctx, tc)
	roots := make([]int, len(panes))
	for i, p := range panes {
		roots[i] = p.PID
	}
	trees := budget.Trees(roots)
	for _, p := range panes {
		unit := cgroup.PaneUnit(p.ID)
		var pids []int
		for _, pid := range trees[p.PID] {
			if fromTmux(cgroup.Unit(pid)) {
				pids = append(pids, pid)
			}
		}
		if len(pids) == 0 {
			continue
		}
		if err := cgroup.Place(ctx, unit, cgroup.PaneSlice, cgroup.PaneWeight(p.Role), pids); err != nil {
			report.Failed = append(report.Failed, p.ID+": "+err.Error())
			continue
		}
		for _, pid := range pids {
			_ = cgroup.RaiseOOMScore(pid, cgroup.PaneOOMScore)
		}
		report.Panes++
		report.Processes += len(pids)
	}
	return report
}

// fromTmux reports whether a process in unit still sits where the tmux
// server started it.
func fromTmux(unit string) bool {
	return unit == cgroup.DaemonUnit || unit == cgroup.TmuxUnit || strings.HasPrefix(unit, "tmux-spawn-")
}

func memTotal() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			return kb * 1024, err
		}
	}
	return 0, fmt.Errorf("no MemTotal in /proc/meminfo")
}
