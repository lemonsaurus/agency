package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/ipc"
	"github.com/lemonsaurus/agency/internal/manifest"
	"github.com/lemonsaurus/agency/internal/restore"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// runRestore is `agency restore`: it runs on the sky host, over SSH from
// earth.
func runRestore(args []string) {
	cfg := loadConfig()
	if cfg.Cloud.Host != "" {
		if err := (&cloud.Client{Host: cfg.Cloud.Host}).Interactive(context.Background(), append([]string{"restore"}, args...)...); err != nil {
			os.Exit(1)
		}
		return
	}
	runHostRestore(args)
}

// runHostRestore rebuilds a dead session from its manifest.
func runHostRestore(args []string) {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	dry := flags.Bool("dry-run", false, "Show what would come back; start nothing")
	skip := flags.String("skip", "", "Comma-separated pane IDs or label fragments to leave dead")
	forget := flags.Bool("forget", false, "Drop the saved panes and any restore in progress")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency restore [--dry-run] [--skip %id,label,...] [--forget]")
		os.Exit(1)
	}
	lipgloss.SetHasDarkBackground(true)
	cfg := loadConfig()
	name := cfg.Session.Name
	os.Setenv("AGENCY_TMUX_SOCKET", "agency-"+name)
	path, pendingFile := manifestPath(name), pendingPath(name)
	if *forget {
		_ = os.Remove(path)
		if err := manifest.Finish(path, pendingFile, ""); err != nil {
			fail("Error: %v", err)
		}
		fmt.Println("Forgot the saved panes.")
		return
	}
	tc := tmux.NewClient(name, "")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if !*dry {
		unlock, err := manifest.Lock(pendingFile)
		if err != nil {
			fail("Error: %v", err)
		}
		defer unlock()
	}
	server, _ := tc.ServerID(ctx)
	state, ok, err := manifest.Freeze(path, pendingFile, server, *dry)
	if err != nil {
		fail("Error: %v", err)
	}
	if !ok {
		fmt.Println("Nothing to restore: the session is the one that was saved.")
		return
	}
	options := restore.Options{}
	if *skip != "" {
		options.Skip = strings.Split(*skip, ",")
	}
	steps := restore.Plan(state.Manifest, options)
	view := newRestoreView(os.Stdout, steps, state, *dry)
	if *dry {
		view.finish(restore.Result{}, true)
		return
	}
	if server == "" {
		fail("Error: the tmux server is not running; the daemon starts one and restores on its own, or restart agency.service.")
	}
	target := hostTarget{tc: tc, socket: socketPath(name)}
	result := restore.Run(ctx, target, state.Manifest, steps, &state,
		func(p manifest.Pending) error { return manifest.Save(pendingFile, p) }, view.event)
	if result.Ok() {
		_ = manifest.Finish(path, pendingFile, server)
	}
	view.finish(result, false)
	if !result.Ok() {
		os.Exit(1)
	}
}

// hostTarget restores through the running daemon.
type hostTarget struct {
	tc     *tmux.Client
	socket string
}

func (t hostTarget) Spawn(ctx context.Context, spec control.RestoreSpec) (string, error) {
	payload, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	reply, err := ipc.SendMessageContext(ctx, t.socket, "restore-pane:"+string(payload))
	if err != nil {
		return "", err
	}
	if message, failed := strings.CutPrefix(reply, "error: "); failed {
		return "", fmt.Errorf("%s", message)
	}
	return reply, nil
}

func (t hostTarget) Alive(ctx context.Context, id, old string) bool {
	return paneRestoredFrom(ctx, t.tc, id, old)
}

func (t hostTarget) Notify(ctx context.Context, id, text string) error {
	return restore.BridgeNotifier(t.socket)(ctx, id, text)
}

var (
	restoreGlyph = map[restoreState]string{
		stateWaiting: "○", stateStarting: "", stateDone: "✔", stateKept: "↺", stateFailed: "✖", stateSkipped: "·",
	}
	spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
)

type restoreState int

const (
	stateWaiting restoreState = iota
	stateStarting
	stateDone
	stateKept
	stateFailed
	stateSkipped
)

type restoreRow struct {
	step  restore.Step
	state restoreState
	id    string
	told  string // "", "telling", "told", "unsent"
	err   error
}

// restoreView draws the step runner: a row per pane and a progress bar. Off a
// terminal it prints one line per event instead.
type restoreView struct {
	mu      sync.Mutex
	out     io.Writer
	tty     bool
	width   int
	height  int
	rows    []*restoreRow
	byOld   map[string]*restoreRow
	dry     bool
	state   manifest.Pending
	drawn   int
	frame   int
	started time.Time
	stop    chan struct{}
}

func newRestoreView(out io.Writer, steps []restore.Step, state manifest.Pending, dry bool) *restoreView {
	v := &restoreView{out: out, dry: dry, state: state, byOld: map[string]*restoreRow{}, started: time.Now(), stop: make(chan struct{}), width: 100, height: 40}
	if file, ok := out.(*os.File); ok && term.IsTerminal(file.Fd()) {
		v.tty = true
		if w, h, err := term.GetSize(file.Fd()); err == nil && w > 0 {
			v.width, v.height = w, h
		}
	}
	for _, step := range steps {
		row := &restoreRow{step: step}
		if step.Skip != "" {
			row.state = stateSkipped
		}
		v.rows = append(v.rows, row)
		v.byOld[step.Pane.ID] = row
	}
	if !v.tty {
		return v
	}
	fmt.Fprint(out, "\x1b[?25l")
	v.draw()
	if !dry {
		go v.animate()
	}
	return v
}

func (v *restoreView) animate() {
	ticker := time.NewTicker(90 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-v.stop:
			return
		case <-ticker.C:
			v.mu.Lock()
			v.frame++
			v.draw()
			v.mu.Unlock()
		}
	}
}

// event records progress from the restore run.
func (v *restoreView) event(e restore.Event) {
	v.mu.Lock()
	defer v.mu.Unlock()
	row := v.byOld[e.Step.Pane.ID]
	switch e.Kind {
	case restore.Starting:
		row.state = stateStarting
	case restore.Started:
		row.state, row.id = stateDone, e.New
	case restore.Kept:
		row.state, row.id = stateKept, e.New
	case restore.Failed:
		row.state, row.err = stateFailed, e.Err
	case restore.Telling:
		row.told = "telling"
	case restore.Told:
		row.told = "told"
	case restore.Unsent:
		row.told, row.err = "unsent", e.Err
	}
	if v.tty {
		v.draw()
		return
	}
	switch e.Kind {
	case restore.Started:
		fmt.Fprintf(v.out, "%s -> %s  %s %s\n", e.Step.Pane.ID, e.New, e.Step.Pane.Role, e.Step.Pane.Label)
	case restore.Kept:
		fmt.Fprintf(v.out, "%s -> %s  already back\n", e.Step.Pane.ID, e.New)
	case restore.Failed, restore.Unsent:
		fmt.Fprintf(v.out, "%s %s: %v\n", e.Step.Pane.ID, e.Kind, e.Err)
	case restore.Told:
		fmt.Fprintf(v.out, "%s told\n", e.New)
	}
}

func (v *restoreView) counts() (done, total int) {
	for _, row := range v.rows {
		if row.state == stateSkipped {
			continue
		}
		total++
		if row.state == stateDone || row.state == stateKept || row.state == stateFailed {
			done++
		}
	}
	return
}

// draw repaints the frame in place.
func (v *restoreView) draw() {
	lines := v.lines()
	var b strings.Builder
	if v.drawn > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", v.drawn)
	}
	for _, line := range lines {
		b.WriteString("\x1b[2K" + line + "\n")
	}
	if extra := v.drawn - len(lines); extra > 0 {
		for i := 0; i < extra; i++ {
			b.WriteString("\x1b[2K\n")
		}
		fmt.Fprintf(&b, "\x1b[%dA", extra)
	}
	v.drawn = len(lines)
	io.WriteString(v.out, b.String())
}

var (
	rowTitle = lipgloss.NewStyle().Foreground(colorSky).Bold(true)
	rowDim   = lipgloss.NewStyle().Foreground(colorDim)
	rowFaint = lipgloss.NewStyle().Foreground(colorFaint)
	rowPlain = lipgloss.NewStyle().Foreground(colorPlain)
	rowGood  = lipgloss.NewStyle().Foreground(colorGreen)
	rowWarn  = lipgloss.NewStyle().Foreground(colorYellow)
	rowBad   = lipgloss.NewStyle().Foreground(colorRed).Bold(true)
	rowRole  = map[string]lipgloss.Style{
		"controller": lipgloss.NewStyle().Foreground(colorPeach).Bold(true),
		"manager":    lipgloss.NewStyle().Foreground(colorSky),
		"worker":     lipgloss.NewStyle().Foreground(colorGreen),
	}
)

// lines is the whole frame, one string per terminal line.
func (v *restoreView) lines() []string {
	idWidth, labelWidth := 0, 0
	for _, row := range v.rows {
		idWidth = max(idWidth, len(row.step.Pane.ID))
		labelWidth = max(labelWidth, lipgloss.Width(rowLabel(row.step.Pane)))
	}
	labelWidth = min(labelWidth, max(v.width*40/100, 20))
	done, total := v.counts()
	title := "agency restore"
	if v.dry {
		title += "  dry run"
	}
	saved := ""
	if !v.state.Manifest.SavedAt.IsZero() {
		saved = "saved " + v.state.Manifest.SavedAt.Local().Format("15:04:05")
	}
	lines := []string{
		"",
		"  " + rowTitle.Render("↺ "+title) + "  " + rowDim.Render(fmt.Sprintf("%s · %d panes · %s", v.state.Manifest.Session, total, saved)),
		"",
	}
	room := max(v.height-len(lines)-4, 5)
	first := 0
	if len(v.rows) > room {
		current := 0
		for i, row := range v.rows {
			if row.state == stateStarting || row.told == "telling" {
				current = i
			}
		}
		first = min(max(current-room/2, 0), len(v.rows)-room)
	}
	if first > 0 {
		lines = append(lines, "  "+rowFaint.Render(fmt.Sprintf("… %d above", first)))
	}
	for i := first; i < len(v.rows) && i < first+room; i++ {
		lines = append(lines, v.row(v.rows[i], idWidth, labelWidth))
	}
	if below := len(v.rows) - (first + room); below > 0 {
		lines = append(lines, "  "+rowFaint.Render(fmt.Sprintf("… %d below", below)))
	}
	lines = append(lines, "", "  "+v.bar(done, total))
	return lines
}

// row is one pane: state, old ID, new ID, role, label, command.
func (v *restoreView) row(row *restoreRow, idWidth, labelWidth int) string {
	pane := row.step.Pane
	glyph, style := restoreGlyph[row.state], rowDim
	switch row.state {
	case stateStarting:
		glyph, style = spinner[v.frame%len(spinner)], rowWarn
	case stateDone:
		style = rowGood
	case stateKept:
		style = rowGood
	case stateFailed:
		style = rowBad
	case stateSkipped:
		style = rowFaint
	}
	switch row.told {
	case "telling":
		glyph, style = spinner[v.frame%len(spinner)], rowWarn
	case "unsent":
		style = rowWarn
	}
	newID := strings.Repeat(" ", idWidth)
	if row.id != "" {
		newID = fmt.Sprintf("%-*s", idWidth, row.id)
	}
	role := rowRole[pane.Role]
	label := rowLabel(pane)
	detail := ""
	switch {
	case row.state == stateSkipped:
		detail = row.step.Skip
	case row.err != nil:
		detail = row.err.Error()
	case row.told == "telling":
		detail = "waiting for Pi to take its recovery note"
	case row.step.Resumed:
		detail = "resumes " + shortPath(pane.Session)
	case isPiCommand(pane.Command):
		detail, style = "no session file: starts a fresh Pi", rowWarn
	default:
		detail = row.step.Command
	}
	arrow := " → "
	if row.id == "" {
		arrow = "   "
		newID = strings.Repeat(" ", idWidth)
	}
	prefix := fmt.Sprintf("  %s  %*s%s%s  %s ", style.Render(glyph), idWidth, pane.ID, rowDim.Render(arrow), style.Render(newID), role.Render(fmt.Sprintf("%-10s", pane.Role)))
	room := max(v.width-lipgloss.Width(prefix)-2, 20)
	labelWidth = min(labelWidth, room*55/100)
	label = truncate(label, labelWidth)
	label += strings.Repeat(" ", max(labelWidth-lipgloss.Width(label), 0))
	return prefix + rowPlain.Render(label) + "  " + style.Render(truncate(detail, max(room-labelWidth-2, 0)))
}

// rowLabel is what a pane is called on screen.
func rowLabel(pane manifest.Pane) string {
	if pane.Label != "" {
		return pane.Label
	}
	return pane.Command
}

func isPiCommand(command string) bool {
	fields := strings.Fields(command)
	return len(fields) > 0 && (fields[0] == "pi" || strings.HasSuffix(fields[0], "/pi"))
}

// bar is the progress bar and count.
func (v *restoreView) bar(done, total int) string {
	const width = 40
	filled := 0
	if total > 0 {
		filled = done * width / total
	}
	bar := rowGood.Render(strings.Repeat("━", filled)) + rowFaint.Render(strings.Repeat("━", width-filled))
	return bar + " " + rowPlain.Render(strconv.Itoa(done)+"/"+strconv.Itoa(total))
}

// finish stops the animation and prints the outcome.
func (v *restoreView) finish(result restore.Result, dry bool) {
	close(v.stop)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.tty {
		v.draw()
		fmt.Fprint(v.out, "\x1b[?25h")
	}
	if dry {
		resumed, fresh, skipped := 0, 0, 0
		for _, row := range v.rows {
			switch {
			case row.step.Skip != "":
				skipped++
			case row.step.Resumed:
				resumed++
			default:
				fresh++
			}
		}
		fmt.Fprintf(v.out, "\n  %s\n\n", rowDim.Render(fmt.Sprintf("Would start %d panes: %d resume a Pi conversation, %d rerun their command. %d stay dead. Nothing was started.", resumed+fresh, resumed, fresh, skipped)))
		return
	}
	switch {
	case result.Ok():
		fmt.Fprintf(v.out, "\n  %s\n\n", rowGood.Render(fmt.Sprintf("✔ %d panes back in %s.", len(result.Restored), time.Since(v.started).Round(time.Second))))
	default:
		fmt.Fprintf(v.out, "\n  %s\n  %s\n\n", rowBad.Render(fmt.Sprintf("✖ %d panes did not come back, %d notes unsent.", len(result.Failed), len(result.Unsent))), rowDim.Render("Run agency restore again to finish what is left."))
	}
}

// shortPath is the last two parts of a path.
func shortPath(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}
