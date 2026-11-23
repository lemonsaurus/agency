package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"github.com/lemonsaurus/agency/internal/budget"
	"github.com/lemonsaurus/agency/internal/cgroup"
	"github.com/lemonsaurus/agency/internal/ipc"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// jobPoll is how often a waiting job asks again; a running one heartbeats
// at jobHeartbeat, well inside the budget's 30 s lease.
const (
	jobPoll      = 2 * time.Second
	jobHeartbeat = 10 * time.Second
)

func runBudget(args []string) {
	cfg := loadConfig()
	switch {
	case len(args) > 0 && args[0] == "cancel":
		if len(args) != 2 {
			fail("Usage: agency budget cancel <ticket>")
		}
		reply, err := budgetCall(cfg.Session.Name, "budget-cancel:"+args[1])
		if err != nil {
			fail("Error: %v", err)
		}
		fmt.Println(reply)
	case len(args) > 0 && args[0] == "scopes":
		tc := tmux.NewClient(cfg.Session.Name, "")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		report, err := applyScopes(ctx, tc)
		if err != nil {
			fail("Error: %v", err)
		}
		fmt.Println(report)
	case len(args) > 0 && args[0] == "--json":
		snapshot, live := readBudget(cfg.Session.Name)
		data, _ := json.MarshalIndent(budgetFeed(snapshot, live), "", "  ")
		fmt.Println(string(data))
	case len(args) == 0:
		snapshot, live := readBudget(cfg.Session.Name)
		width, _, err := term.GetSize(os.Stdout.Fd())
		if err != nil || width <= 0 {
			width = 100
		}
		fmt.Print(renderBudget(snapshot, live, width, 12))
	default:
		fail("Usage: agency budget [--json | cancel <ticket> | scopes]")
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func budgetCall(sessionName, message string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply, err := ipc.SendMessageContext(ctx, socketPath(sessionName), message)
	if err != nil {
		return "", err
	}
	if rest, ok := strings.CutPrefix(reply, "error: "); ok {
		return "", errors.New(rest)
	}
	return reply, nil
}

// readBudget asks the daemon for its budget. A daemon without one gets a
// local reading of the box and panes instead, with live false.
func readBudget(sessionName string) (budget.Snapshot, bool) {
	if reply, err := budgetCall(sessionName, "budget"); err == nil {
		var snapshot budget.Snapshot
		if json.Unmarshal([]byte(reply), &snapshot) == nil {
			return snapshot, true
		}
	}
	tc := tmux.NewClient(sessionName, "")
	b := budget.New(budget.DefaultLimits, "")
	b.Panes = func(ctx context.Context) []budget.Pane { return budgetPanes(ctx, tc) }
	ctx := context.Background()
	b.Sample(ctx)
	time.Sleep(time.Second)
	b.Sample(ctx)
	return b.Snapshot(), false
}

// feedEntry is one pane or job in `agency budget --json`.
type feedEntry struct {
	budget.Entry
	Alive     bool             `json:"alive"`
	Processes []budget.Process `json:"processes"`
}

type feed struct {
	Live      bool             `json:"live"` // false: the daemon has no budget, this is a local reading
	At        time.Time        `json:"at"`
	Box       budget.Box       `json:"box"`
	Busy      float64          `json:"busy"`
	Ceiling   budget.Cost      `json:"ceiling"`
	Committed budget.Cost      `json:"committed"`
	Headroom  budget.Cost      `json:"headroom"`
	Backstop  string           `json:"backstop,omitempty"`
	Other     budget.Cost      `json:"other"`
	Queue     []budget.Ticket  `json:"queue"`
	Entries   []feedEntry      `json:"entries"`
	Strays    []budget.Process `json:"strays"` // processes in agency's units that no pane or job holds
}

// budgetFeed adds every process of every entry, and the strays, to a snapshot.
func budgetFeed(s budget.Snapshot, live bool) feed {
	out := feed{
		Live: live, At: s.At, Box: s.Box, Busy: s.Busy, Ceiling: s.Ceiling, Committed: s.Committed,
		Headroom: s.Headroom, Backstop: s.Backstop, Other: s.Other, Queue: s.Queue,
	}
	roots := make([]int, 0, len(s.Entries))
	for _, e := range s.Entries {
		if e.PID > 0 {
			roots = append(roots, e.PID)
		}
	}
	trees := budget.Trees(roots)
	keep := map[int]bool{os.Getpid(): true}
	var all []int
	for _, root := range roots {
		for _, pid := range trees[root] {
			keep[pid] = true
			all = append(all, pid)
		}
	}
	for _, pid := range []int{daemonPID(), tmuxServerPID()} {
		keep[pid] = true
	}
	strays := budget.Strays(cgroup.Unit, agencyUnit, keep)
	processes := map[int]budget.Process{}
	for _, p := range budget.Inspect(append(all, strays...), time.Second) {
		processes[p.PID] = p
	}
	for _, e := range s.Entries {
		entry := feedEntry{Entry: e, Processes: []budget.Process{}}
		for _, pid := range trees[e.PID] {
			if p, ok := processes[pid]; ok {
				entry.Processes = append(entry.Processes, p)
			}
		}
		entry.Alive = len(entry.Processes) > 0
		out.Entries = append(out.Entries, entry)
	}
	out.Strays = []budget.Process{}
	for _, pid := range strays {
		if p, ok := processes[pid]; ok {
			out.Strays = append(out.Strays, p)
		}
	}
	return out
}

func agencyUnit(unit string) bool {
	return unit == cgroup.DaemonUnit || unit == cgroup.TmuxUnit ||
		strings.HasPrefix(unit, "agency-pane-") || strings.HasPrefix(unit, "agency-job-")
}

func daemonPID() int {
	out, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", cgroup.DaemonUnit).Output()
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &pid)
	return pid
}

func tmuxServerPID() int {
	out, err := tmux.NewClient("", "").Cmd.Run(context.Background(), "display-message", "-p", "#{pid}")
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(out), "%d", &pid)
	return pid
}

// Budget view styles, from the palette deaths.go defines.
var (
	budgetTitle = lipgloss.NewStyle().Foreground(colorSky).Bold(true)
	budgetDim   = lipgloss.NewStyle().Foreground(colorDim)
	budgetFaint = lipgloss.NewStyle().Foreground(colorFaint)
	budgetPlain = lipgloss.NewStyle().Foreground(colorPlain)
	budgetBold  = lipgloss.NewStyle().Foreground(colorPlain).Bold(true)
	budgetPane  = lipgloss.NewStyle().Foreground(colorBlueText).Bold(true)
	budgetGood  = lipgloss.NewStyle().Foreground(colorGreen)
	budgetWarn  = lipgloss.NewStyle().Foreground(colorYellow)
	budgetBad   = lipgloss.NewStyle().Foreground(colorRed).Bold(true)
	budgetJob   = lipgloss.NewStyle().Foreground(colorPeach).Bold(true)
	budgetRole  = map[string]lipgloss.Style{
		"controller": lipgloss.NewStyle().Foreground(colorLavender),
		"manager":    lipgloss.NewStyle().Foreground(colorSky),
		"worker":     lipgloss.NewStyle().Foreground(colorGreen),
	}
)

// Hog lines: a pane past one is flagged in the view.
const (
	hogCPU  = 1.5      // cores, smoothed
	hogMem  = 4 * 1024 // MB
	idleFor = 6 * time.Hour
)

// hogs names what makes an entry stand out.
func hogs(e budget.Entry, now time.Time) []string {
	var flags []string
	if e.Used.CPU >= hogCPU {
		flags = append(flags, "cpu")
	}
	if e.Used.Mem>>20 >= hogMem {
		flags = append(flags, "mem")
	}
	if e.Kind == budget.KindPane && !e.Activity.IsZero() && now.Sub(e.Activity) > idleFor && e.Used.CPU >= 0.2 {
		flags = append(flags, "idle-burn")
	}
	return flags
}

// gauge draws used against total with the ceiling marked.
func gauge(used, ceiling, total float64, width int) string {
	if total <= 0 || width < 4 {
		return ""
	}
	fill := int(min(used/total, 1) * float64(width))
	mark := int(min(ceiling/total, 1) * float64(width))
	style := budgetGood
	switch {
	case used > ceiling:
		style = budgetBad
	case used > 0.85*ceiling:
		style = budgetWarn
	}
	var b strings.Builder
	for i := range width {
		switch {
		case i == mark:
			b.WriteString(budgetPlain.Render("│"))
		case i < fill:
			b.WriteString(style.Render("█"))
		default:
			b.WriteString(budgetFaint.Render("░"))
		}
	}
	return b.String()
}

func pad(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
}

func padLeft(s string, width int) string {
	return strings.Repeat(" ", max(0, width-lipgloss.Width(s))) + s
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func gb(b uint64) string { return fmt.Sprintf("%.1f", float64(b)/budget.GB) }

// renderBudget draws the budget: gauges, queue, jobs, and the top panes.
func renderBudget(s budget.Snapshot, live bool, width, rows int) string {
	width = max(width, 72)
	now := s.At
	box := s.Box
	var b strings.Builder

	title := budgetTitle.Render("⚖ budget")
	sub := budgetDim.Render(fmt.Sprintf("  %d cores · %s GB", box.Cores, gb(box.MemTotal)))
	if !live {
		sub += budgetWarn.Render("  · local reading, daemon has no budget yet")
	}
	clock := budgetFaint.Render(now.Format("15:04:05"))
	b.WriteString(" " + title + sub + strings.Repeat(" ", max(1, width-lipgloss.Width(title+sub+clock)-2)) + clock + "\n\n")

	barWidth := max(20, width-48)
	ceilingShare := budget.DefaultLimits.Share
	cpuTotal, memTotal := float64(box.Cores), float64(box.MemTotal)
	fmt.Fprintf(&b, " %s %s %s\n", budgetDim.Render("cpu"), gauge(s.Committed.CPU, s.Ceiling.CPU, cpuTotal, barWidth),
		budgetPlain.Render(fmt.Sprintf("%5.1f / %.1f cores", s.Committed.CPU, s.Ceiling.CPU))+budgetFaint.Render(fmt.Sprintf("  ceiling %.0f%%", ceilingShare*100)))
	fmt.Fprintf(&b, " %s %s %s\n", budgetDim.Render("mem"), gauge(float64(s.Committed.Mem), float64(s.Ceiling.Mem), memTotal, barWidth),
		budgetPlain.Render(fmt.Sprintf("%5s / %s GB", gb(s.Committed.Mem), gb(s.Ceiling.Mem))))

	status := budgetGood.Render("✓ admitting")
	switch {
	case s.Backstop != "":
		status = budgetBad.Render("✗ backstop: " + s.Backstop)
	case s.Headroom.CPU < budget.DefaultPaneCost.CPU || s.Headroom.Mem < budget.DefaultPaneCost.Mem:
		status = budgetWarn.Render("● full")
	}
	stats := fmt.Sprintf("load %.1f · psi cpu %.0f%% mem %.1f%% io %.1f%% · %s GB free · disk %.0f%%",
		box.Load1, box.CPU.SomeAvg10, box.Memory.SomeAvg60, box.IO.SomeAvg60, gb(box.MemAvailable), box.DiskUsed)
	fmt.Fprintf(&b, " %s  %s\n", status, budgetDim.Render(stats))
	fmt.Fprintf(&b, " %s\n", budgetDim.Render(fmt.Sprintf("headroom %.1f cores, %s GB · outside agency %.1f cores, %s GB",
		s.Headroom.CPU, gb(s.Headroom.Mem), s.Other.CPU, gb(s.Other.Mem))))

	section := func(name string, count int) {
		label := budgetBold.Render(fmt.Sprintf("%s (%d)", name, count))
		b.WriteString("\n " + label + " " + budgetFaint.Render(strings.Repeat("─", max(0, width-lipgloss.Width(label)-3))) + "\n")
	}
	role := func(r string) string {
		style, ok := budgetRole[r]
		if !ok {
			style = budgetDim
		}
		return pad(style.Render(r), 10)
	}

	if len(s.Queue) > 0 {
		section("queue", len(s.Queue))
		for i, t := range s.Queue {
			who := string(t.Role)
			if t.Human {
				who = "lemon"
			}
			kind := budgetPane.Render(pad(t.ID, 6))
			if t.Kind == budget.KindJob {
				kind = budgetJob.Render(pad(t.ID, 6))
			}
			fmt.Fprintf(&b, " %s %s %s %s %s %s %s\n",
				budgetFaint.Render(padLeft(fmt.Sprint(i+1), 2)), kind, role(who),
				budgetDim.Render(pad(truncate(t.Pane, 6), 6)),
				pad(budgetPlain.Render(truncate(firstNonEmpty(t.Label, t.Class), width-62)), width-62),
				budgetDim.Render(padLeft(fmt.Sprintf("%.1fc %sG", t.Cost.CPU, gb(t.Cost.Mem)), 11)),
				budgetFaint.Render(padLeft(ago(now.Sub(t.Enqueued)), 5)))
		}
	}

	var jobs, panes []budget.Entry
	for _, e := range s.Entries {
		if e.Kind == budget.KindJob {
			jobs = append(jobs, e)
		} else {
			panes = append(panes, e)
		}
	}
	row := func(e budget.Entry, name string) {
		flags := hogs(e, now)
		flag := ""
		if len(flags) > 0 {
			flag = budgetBad.Render(" " + strings.Join(flags, " "))
		}
		labelWidth := width - 64
		fmt.Fprintf(&b, " %s %s %s %s %s %s %s%s\n",
			name, role(string(e.Role)),
			pad(budgetPlain.Render(truncate(firstNonEmpty(e.Label, e.Class), labelWidth)), labelWidth),
			budgetPlain.Render(padLeft(fmt.Sprintf("%.2fc", e.Used.CPU), 6)),
			budgetPlain.Render(padLeft(gb(e.Used.Mem)+"G", 6)),
			budgetDim.Render(padLeft(fmt.Sprintf("%d proc", e.Procs), 8)),
			budgetFaint.Render(padLeft(fmt.Sprintf("est %.1fc %sG", e.Estimate.CPU, gb(e.Estimate.Mem)), 14)),
			flag)
	}
	if len(jobs) > 0 {
		section("jobs", len(jobs))
		for _, e := range jobs {
			row(e, budgetJob.Render(pad(e.ID, 6)))
		}
	}
	sort.SliceStable(panes, func(i, j int) bool {
		return panes[i].Used.CPU+float64(panes[i].Used.Mem)/budget.GB > panes[j].Used.CPU+float64(panes[j].Used.Mem)/budget.GB
	})
	section("panes", len(panes))
	for i, e := range panes {
		if i == rows {
			b.WriteString(budgetFaint.Render(fmt.Sprintf("   … %d more\n", len(panes)-rows)))
			break
		}
		row(e, budgetPane.Render(pad(e.ID, 6)))
	}
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// runJob runs a heavy command once the box has budget for it.
func runJob(args []string) {
	cfg := loadConfig()
	var request ipc.JobRequest
	for len(args) > 0 && args[0] != "--" {
		if len(args) < 2 {
			fail("Usage: agency job [--cost 4c,6g] [--label text] -- <command...>")
		}
		switch args[0] {
		case "--cost":
			cost, err := budget.ParseCost(args[1])
			if err != nil {
				fail("Error: %v", err)
			}
			request.Cost = cost
		case "--label":
			request.Label = args[1]
		default:
			fail("Error: unknown job option %s", args[0])
		}
		args = args[2:]
	}
	if len(args) < 2 {
		fail("Usage: agency job [--cost 4c,6g] [--label text] -- <command...>")
	}
	command := args[1:]
	request.Class = jobClass(command)
	request.PID = os.Getpid()
	if request.Label == "" {
		request.Label = truncate(strings.Join(command, " "), 60)
	}

	interrupted := make(chan os.Signal, 2)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	ticket, ok := waitForBudget(cfg.Session.Name, &request, interrupted)
	if ok {
		if cgroup.Active() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = cgroup.Place(ctx, cgroup.JobUnit(ticket), cgroup.JobSlice, 100, []int{os.Getpid()})
			cancel()
		}
		stop := make(chan struct{})
		go heartbeat(cfg.Session.Name, request, stop)
		code := runCommand(command, interrupted)
		close(stop)
		_, _ = budgetCall(cfg.Session.Name, "job-done:"+ticket)
		os.Exit(code)
	}
	os.Exit(runCommand(command, interrupted))
}

// waitForBudget polls until the daemon admits the job. ok is false when the
// daemon cannot budget, and the job then runs without it.
func waitForBudget(sessionName string, request *ipc.JobRequest, interrupted chan os.Signal) (string, bool) {
	tty := term.IsTerminal(os.Stderr.Fd())
	shown := ""
	ticket := ""
	for {
		data, _ := json.Marshal(request)
		reply, err := budgetCall(sessionName, "job:"+string(data))
		var d budget.Decision
		if err == nil {
			err = json.Unmarshal([]byte(reply), &d)
		}
		if err != nil {
			if ticket == "" {
				fmt.Fprintf(os.Stderr, "agency job: no budget from the daemon (%v); running now\n", err)
				return "", false
			}
			// A restarting daemon forgets the ticket; keep asking with it.
			d = budget.Decision{Ticket: ticket, Reason: "daemon unreachable: " + err.Error()}
		}
		ticket = d.Ticket
		request.Ticket = ticket
		if d.Admitted {
			if shown != "" && tty {
				fmt.Fprint(os.Stderr, "\r\033[K")
			}
			if shown != "" {
				fmt.Fprintf(os.Stderr, "agency job %s: budget granted, starting\n", ticket)
			}
			return ticket, true
		}
		line := fmt.Sprintf("⏳ agency job %s waiting for budget · position %d of %d · needs %s · %s",
			ticket, d.Position, d.Waiting, d.Cost, d.Reason)
		switch {
		case tty:
			fmt.Fprint(os.Stderr, "\r\033[K"+truncate(line, 200))
		case line != shown:
			fmt.Fprintln(os.Stderr, line)
		}
		shown = line
		select {
		case <-interrupted:
			_, _ = budgetCall(sessionName, "job-done:"+ticket)
			if tty {
				fmt.Fprintln(os.Stderr)
			}
			os.Exit(130)
		case <-time.After(jobPoll):
		}
	}
}

func heartbeat(sessionName string, request ipc.JobRequest, stop chan struct{}) {
	ticker := time.NewTicker(jobHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			data, _ := json.Marshal(request)
			_, _ = budgetCall(sessionName, "job:"+string(data))
		}
	}
}

// runCommand runs argv with the terminal and passes signals on. It returns
// the exit code, 128+signal for a killed command.
func runCommand(argv []string, interrupted chan os.Signal) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "agency job: %v\n", err)
		return 127
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-interrupted:
				_ = cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}

// jobClass names a job by its program, first subcommand, and repository, so
// `cargo build` in legal-drive learns apart from `cargo test` elsewhere.
func jobClass(argv []string) string {
	words := []string{filepath.Base(argv[0])}
	for _, arg := range argv[1:] {
		if !strings.HasPrefix(arg, "-") {
			words = append(words, filepath.Base(arg))
			break
		}
	}
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		words = append(words, "@"+filepath.Base(strings.TrimSpace(string(out))))
	}
	return strings.Join(words, " ")
}
