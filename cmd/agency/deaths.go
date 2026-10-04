package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"

	"github.com/lemonsaurus/agency/internal/deaths"
	"github.com/lemonsaurus/agency/internal/oomguard"
	"github.com/lemonsaurus/agency/internal/session"
)

// paneExitFormat is what the pane-died hook reads from the dead pane before killing it.
const paneExitFormat = "#{pane_pid}\t#{pane_dead_status}\t#{pane_dead_signal}\t#{pane_start_path}\t#{@agency_task_label}\t#{@agency_role}\t#{@agency_group}\t#{window_name}\t#{@agency_command}\t#{pane_start_command}"

// settle is how long follow mode waits for a death's later events (the
// daemon's gone, the OOM probe) before printing its story.
const settle = 8 * time.Second

// startDeathLog points the manager at the death log, notes the daemon start,
// and keeps systemd's OOM handling from killing whole panes.
func startDeathLog(mgr *session.Manager, cloud bool) {
	deathLog := deaths.Open()
	mgr.Deaths = deathLog
	note := "interactive"
	if cloud {
		note = "headless"
	}
	deathLog.Record(deaths.Event{Kind: deaths.KindDaemon, PID: os.Getpid(), Note: note})
	if !oomguard.Active() {
		return
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		log.Printf("Warning: OOM guard: %v", err)
		return
	}
	written, err := oomguard.Install(configDir, oomguard.Reload)
	if err != nil {
		log.Printf("Warning: OOM guard: %v", err)
	}
	if len(written) > 0 {
		log.Printf("OOM guard installed: %s", strings.Join(written, ", "))
		deathLog.Record(deaths.Event{Kind: deaths.KindGuard, Note: "OOMPolicy=continue for " + strings.Join(guardUnits(written), " and ")})
	}
}

func guardUnits(paths []string) []string {
	units := make([]string, 0, len(paths))
	for _, path := range paths {
		units = append(units, strings.TrimSuffix(filepath.Base(filepath.Dir(path)), ".d"))
	}
	return units
}

// runPaneExit is the tmux pane-died and pane-exited hook. It records the
// death and kills the dead pane that remain-on-exit kept for it to read.
func runPaneExit(args []string) {
	if len(args) != 3 || args[0] != deaths.KindDied && args[0] != deaths.KindExited {
		fmt.Fprintln(os.Stderr, "Usage: agency pane-exit died|exited <tmux-socket> <pane-id>")
		os.Exit(1)
	}
	kind, socket, pane := args[0], args[1], args[2]
	deathLog := deaths.Open()
	if kind == deaths.KindExited {
		status := 0
		deathLog.Record(deaths.Event{Kind: kind, Pane: pane, Status: &status})
		return
	}
	at := time.Now()
	out, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "-t", pane, paneExitFormat).Output()
	event := paneExitEvent(pane, strings.TrimRight(string(out), "\n"))
	if err != nil {
		event.Note = "tmux had already dropped the pane"
	}
	_ = exec.Command("tmux", "-S", socket, "kill-pane", "-t", pane).Run()
	event.At = at
	event.Memory = deaths.Memory()
	if event.Signal == "SIGKILL" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		event.OOM = deaths.ProbeOOM(ctx, event.PID, at)
		cancel()
	}
	deathLog.Record(event)
}

// paneExitEvent parses paneExitFormat output for a dead pane.
func paneExitEvent(pane, line string) deaths.Event {
	event := deaths.Event{Kind: deaths.KindDied, Pane: pane}
	fields := strings.Split(line, "\t")
	if len(fields) < 10 {
		return event
	}
	event.PID, _ = strconv.Atoi(fields[0])
	if status, err := strconv.Atoi(fields[1]); err == nil {
		event.Status = &status
	}
	event.Signal = deaths.SignalName(fields[2])
	event.Dir = fields[3]
	event.Label = fields[4]
	event.Role = fields[5]
	event.Group = fields[6]
	if event.Group == "" {
		event.Group = fields[7]
	}
	event.Command = fields[9]
	if decoded, err := base64.RawStdEncoding.DecodeString(fields[8]); fields[8] != "" && err == nil {
		event.Command = string(decoded)
	}
	return event
}

// runLog dispatches `agency log <topic>`.
func runLog(args []string) {
	if len(args) == 0 || args[0] != "deaths" {
		fmt.Fprintln(os.Stderr, "Usage: agency log deaths [--since 48h|7d|all] [--pane %N] [--json] [--follow]")
		os.Exit(1)
	}
	flags := flag.NewFlagSet("agency log deaths", flag.ExitOnError)
	sinceFlag := flags.String("since", "48h", "how far back: 90m, 48h, 7d, or all")
	pane := flags.String("pane", "", "only this pane, e.g. %1313")
	asJSON := flags.Bool("json", false, "print stories as JSON")
	follow := flags.Bool("follow", false, "keep printing new deaths")
	flags.BoolVar(follow, "f", false, "short for --follow")
	_ = flags.Parse(args[1:])

	since, err := parseSince(*sinceFlag, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	deathLog := deaths.Open()
	home, _ := os.UserHomeDir()
	activity := filepath.Join(home, ".agents", "run", "agency", "activity.jsonl")
	load := func() []deaths.Story {
		events, err := deathLog.Read(since)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: reading %s: %v\n", deathLog.Path, err)
			os.Exit(1)
		}
		stories := filterPane(deaths.Stories(events), *pane)
		deaths.AttachSteps(stories, deaths.ReadSteps(activity, since))
		return stories
	}

	stories := load()
	r := newDeathRenderer(home)
	if *asJSON && !*follow {
		data, _ := json.MarshalIndent(stories, "", "  ")
		fmt.Println(string(data))
		return
	}
	if !*asJSON {
		fmt.Print(r.header(deathLog.Path, since, stories, *follow))
	}
	printed := map[string]bool{}
	var lastDay string
	emit := func(story deaths.Story) {
		printed[storyKey(story)] = true
		if *asJSON {
			data, _ := json.Marshal(story)
			fmt.Println(string(data))
			return
		}
		if day := story.At.Format("Mon 02 Jan"); day != lastDay {
			fmt.Print(r.day(day))
			lastDay = day
		}
		fmt.Print(r.story(story))
	}
	for _, story := range stories {
		if !*follow || time.Since(story.At) >= settle {
			emit(story)
		}
	}
	if !*follow {
		fmt.Print(r.footer(stories))
		return
	}
	for {
		time.Sleep(time.Second)
		for _, story := range load() {
			if !printed[storyKey(story)] && time.Since(story.At) >= settle {
				emit(story)
			}
		}
	}
}

func storyKey(story deaths.Story) string {
	return story.Pane + "@" + story.At.Format(time.RFC3339Nano)
}

func filterPane(stories []deaths.Story, pane string) []deaths.Story {
	if pane == "" {
		return stories
	}
	var out []deaths.Story
	for _, story := range stories {
		if story.Pane == pane || story.Successor == pane {
			out = append(out, story)
		}
	}
	return out
}

// parseSince turns 90m, 48h, 7d, or all into a cutoff time.
func parseSince(value string, now time.Time) (time.Time, error) {
	if value == "all" {
		return time.Time{}, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return time.Time{}, fmt.Errorf("--since %q: use 90m, 48h, 7d, or all", value)
		}
		return now.AddDate(0, 0, -n), nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return time.Time{}, fmt.Errorf("--since %q: use 90m, 48h, 7d, or all", value)
	}
	return now.Add(-d), nil
}

// Catppuccin Mocha, as in the palette.
var (
	colorRed      = lipgloss.Color("#f38ba8")
	colorPeach    = lipgloss.Color("#fab387")
	colorGreen    = lipgloss.Color("#a6e3a1")
	colorYellow   = lipgloss.Color("#f9e2af")
	colorSky      = lipgloss.Color("#89dceb")
	colorLavender = lipgloss.Color("#cba6f7")
	colorBlueText = lipgloss.Color("#89b4fa")
	colorPlain    = lipgloss.Color("#cdd6f4")
	colorDim      = lipgloss.Color("#7f849c")
	colorFaint    = lipgloss.Color("#585b70")
)

type outcomeStyle struct {
	glyph, word string
	color       lipgloss.Color
}

var outcomeStyles = map[string]outcomeStyle{
	deaths.KindDied:        {"✗", "died", colorRed},
	deaths.KindKilled:      {"✂", "killed", colorPeach},
	deaths.KindExited:      {"✓", "exited", colorGreen},
	deaths.OutcomeVanished: {"?", "vanished", colorYellow},
	deaths.OutcomeHandoff:  {"⇢", "handoff", colorSky},
	deaths.KindDaemon:      {"↻", "daemon", colorLavender},
	deaths.KindGuard:       {"⛨", "guard", colorLavender},
}

const labelWidth = 34

type deathRenderer struct {
	home  string
	width int
	dim   lipgloss.Style
	faint lipgloss.Style
	bold  lipgloss.Style
	pane  lipgloss.Style
	plain lipgloss.Style
}

func newDeathRenderer(home string) deathRenderer {
	width := 120
	if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 40 {
		width = w
	}
	return deathRenderer{
		home:  home,
		width: width,
		dim:   lipgloss.NewStyle().Foreground(colorDim),
		faint: lipgloss.NewStyle().Foreground(colorFaint),
		bold:  lipgloss.NewStyle().Foreground(colorPlain).Bold(true),
		pane:  lipgloss.NewStyle().Foreground(colorBlueText).Bold(true),
		plain: lipgloss.NewStyle().Foreground(colorPlain),
	}
}

func (r deathRenderer) header(path string, since time.Time, stories []deaths.Story, follow bool) string {
	title := lipgloss.NewStyle().Foreground(colorRed).Bold(true).Render("☠ pane deaths")
	window := "all time"
	if !since.IsZero() {
		window = "since " + since.Format("Mon 02 Jan 15:04")
	}
	if follow {
		window += " · following"
	}
	left := " " + title + "  " + r.dim.Render(window)
	right := r.faint.Render(r.tilde(path)) + " "
	gap := r.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		return left + "\n" + " " + right + "\n"
	}
	head := left + strings.Repeat(" ", gap) + right + "\n"
	if len(stories) == 0 && !follow {
		head += "\n " + r.dim.Render("Nothing recorded in this window. Hooks and the OOM guard install when the agency daemon starts.") + "\n"
	}
	return head
}

func (r deathRenderer) day(day string) string {
	rule := r.faint.Render(strings.Repeat("─", 3)) + " " + r.dim.Render(day) + " "
	return "\n " + rule + r.faint.Render(strings.Repeat("─", max(0, r.width-lipgloss.Width(rule)-2))) + "\n"
}

func (r deathRenderer) story(s deaths.Story) string {
	style, ok := outcomeStyles[s.Outcome]
	if !ok {
		style = outcomeStyle{"•", s.Outcome, colorDim}
	}
	badge := lipgloss.NewStyle().Foreground(style.color).Bold(true)
	clock := r.dim.Render(s.At.Format("15:04:05"))
	word := badge.Render(fmt.Sprintf("%s %-8s", style.glyph, style.word))
	indent := strings.Repeat(" ", 13)

	var b strings.Builder
	if s.Pane == "" {
		fmt.Fprintf(&b, "\n %s  %s %s\n", clock, word, r.plain.Render(s.Cause))
		return b.String()
	}
	pane := s.Pane
	if s.Successor != "" {
		pane += " → " + s.Successor
	}
	label := s.Label
	if label == "" {
		label = "(no label)"
		if fields := strings.Fields(s.Command); len(fields) > 0 {
			label = fields[0]
		}
	}
	label = truncate(label, labelWidth)
	meta := strings.Join(nonEmpty(s.Role, s.Group), " · ")
	fmt.Fprintf(&b, "\n %s  %s %s  %s%s  %s\n", clock, word,
		r.pane.Render(fmt.Sprintf("%-14s", pane)),
		r.bold.Render(label), strings.Repeat(" ", labelWidth-lipgloss.Width(label)),
		r.dim.Render(meta))
	b.WriteString(r.detail(indent, "", lipgloss.NewStyle().Foreground(style.color), s.Cause))

	if s.Signal != "" {
		b.WriteString(r.detail(indent, "exit", r.plain, s.Signal))
	}
	if process := strings.Join(nonEmpty(pidText(s.PID), s.Memory), " · "); process != "" {
		b.WriteString(r.detail(indent, "pid", r.dim, process))
	}
	if s.LastStep != nil {
		text := strings.Join(strings.Fields(s.LastStep.Text), " ")
		b.WriteString(r.detail(indent, "last", r.dim, s.LastStep.At.Format("15:04:05")+"  "+truncate(text, r.width-len(indent)-18)))
	}
	if s.Dir != "" {
		b.WriteString(r.detail(indent, "in", r.faint, r.tilde(s.Dir)))
	}
	return b.String()
}

func pidText(pid int) string {
	if pid == 0 {
		return ""
	}
	return strconv.Itoa(pid)
}

func (r deathRenderer) footer(stories []deaths.Story) string {
	counts := map[string]int{}
	oom := 0
	for _, s := range stories {
		counts[s.Outcome]++
		if s.OOM != nil {
			oom++
		}
	}
	var parts []string
	for _, outcome := range []string{deaths.KindDied, deaths.KindKilled, deaths.KindExited, deaths.OutcomeVanished, deaths.OutcomeHandoff} {
		n := counts[outcome]
		if n == 0 {
			continue
		}
		style := outcomeStyles[outcome]
		part := lipgloss.NewStyle().Foreground(style.color).Render(fmt.Sprintf("%d %s", n, style.word))
		if outcome == deaths.KindDied && oom > 0 {
			part += r.dim.Render(fmt.Sprintf(" (%d out of memory)", oom))
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "\n"
	}
	return "\n " + strings.Join(parts, r.faint.Render(" · ")) + "\n\n"
}

// detail renders a story line: an optional dim key column, then text in
// style, word-wrapped to the terminal width under its own first column.
func (r deathRenderer) detail(indent, key string, style lipgloss.Style, text string) string {
	prefix, hang := indent, indent
	if key != "" {
		prefix += r.faint.Render(fmt.Sprintf("%-6s", key))
		hang += strings.Repeat(" ", 6)
	}
	width := max(20, r.width-lipgloss.Width(hang)-1)
	var b strings.Builder
	line := ""
	flush := func() {
		b.WriteString(prefix + style.Render(line) + "\n")
		prefix, line = hang, ""
	}
	for _, word := range strings.Split(text, " ") {
		if line != "" && lipgloss.Width(line)+1+lipgloss.Width(word) > width {
			flush()
		} else if line != "" {
			line += " "
		}
		line += word
	}
	if line != "" {
		flush()
	}
	return b.String()
}

func (r deathRenderer) tilde(path string) string {
	if r.home != "" && strings.HasPrefix(path, r.home+"/") {
		return "~" + path[len(r.home):]
	}
	return path
}

func truncate(text string, width int) string {
	if width < 2 || lipgloss.Width(text) <= width {
		return text
	}
	runes := []rune(text)
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
