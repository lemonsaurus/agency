package deaths

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// mergeWindow joins the events one death leaves behind: the hook's died, the
// daemon's gone a few seconds later, a kill request and its prune.
const mergeWindow = 2 * time.Minute

// Outcomes beyond the event kinds.
const (
	OutcomeVanished = "vanished" // only the daemon's gone event: no hook, no kill request
	OutcomeHandoff  = "handoff"  // replaced by a successor, no death recorded yet
)

var outcomeRank = map[string]int{KindKilled: 4, KindDied: 3, KindExited: 2, KindGone: 1, KindHandoff: 0}

// Story is everything recorded about one pane's end.
type Story struct {
	Pane      string    `json:"pane,omitempty"`
	At        time.Time `json:"at"`
	Outcome   string    `json:"outcome"`
	Cause     string    `json:"cause"`
	Label     string    `json:"label,omitempty"`
	Role      string    `json:"role,omitempty"`
	Group     string    `json:"group,omitempty"`
	Dir       string    `json:"dir,omitempty"`
	Command   string    `json:"command,omitempty"`
	PID       int       `json:"pid,omitempty"`
	Status    *int      `json:"status,omitempty"`
	Signal    string    `json:"signal,omitempty"`
	OOM       *OOM      `json:"oom,omitempty"`
	By        *Actor    `json:"by,omitempty"`
	Successor string    `json:"successor,omitempty"`
	Memory    string    `json:"memory,omitempty"`
	Note      string    `json:"note,omitempty"`
	LastStep  *Step     `json:"lastStep,omitempty"`
	Events    []Event   `json:"events"`
}

// Step is the pane's last activity diary entry before it ended.
type Step struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

// Stories folds events into one story per pane death, oldest first. Daemon
// starts and guard installs stand alone so gaps in coverage show.
func Stories(events []Event) []Story {
	sorted := append([]Event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })

	var stories []Story
	open := map[string]*Story{}
	closeStory := func(pane string) {
		if s := open[pane]; s != nil {
			stories = append(stories, *s)
			delete(open, pane)
		}
	}
	for _, e := range sorted {
		if e.Pane == "" || e.Kind == KindDaemon || e.Kind == KindGuard {
			stories = append(stories, Story{Pane: e.Pane, At: e.At, Outcome: e.Kind, Note: e.Note, PID: e.PID, Events: []Event{e}})
			continue
		}
		s := open[e.Pane]
		if s != nil && s.Outcome != OutcomeHandoff && e.At.Sub(s.At) > mergeWindow {
			closeStory(e.Pane)
			s = nil
		}
		if s == nil {
			s = &Story{Pane: e.Pane, At: e.At, Outcome: OutcomeHandoff}
			open[e.Pane] = s
		}
		merge(s, e)
	}
	for pane := range open {
		closeStory(pane)
	}
	sort.SliceStable(stories, func(i, j int) bool { return stories[i].At.Before(stories[j].At) })
	for i := range stories {
		if stories[i].Outcome == KindGone {
			stories[i].Outcome = OutcomeVanished
		}
		stories[i].Cause = cause(stories[i])
	}
	return stories
}

func merge(s *Story, e Event) {
	s.Events = append(s.Events, e)
	if e.Kind != KindHandoff && (s.Outcome == OutcomeHandoff || outcomeRank[e.Kind] > outcomeRank[s.Outcome]) {
		if s.Outcome == OutcomeHandoff {
			s.At = e.At
		}
		s.Outcome = e.Kind
	}
	fill(&s.Label, e.Label)
	fill(&s.Role, e.Role)
	fill(&s.Group, e.Group)
	fill(&s.Dir, e.Dir)
	fill(&s.Command, e.Command)
	fill(&s.Signal, e.Signal)
	fill(&s.Successor, e.Successor)
	fill(&s.Memory, e.Memory)
	fill(&s.Note, e.Note)
	if s.PID == 0 {
		s.PID = e.PID
	}
	if s.Status == nil {
		s.Status = e.Status
	}
	if s.OOM == nil {
		s.OOM = e.OOM
	}
	if s.By == nil {
		s.By = e.By
	}
}

func fill(field *string, value string) {
	if *field == "" {
		*field = value
	}
}

func cause(s Story) string {
	switch s.Outcome {
	case KindKilled:
		text := "killed by " + DescribeActor(s.By)
		if s.Note != "" {
			text += " (" + s.Note + ")"
		}
		return text
	case KindDied:
		switch {
		case s.OOM != nil && s.OOM.Exact:
			return fmt.Sprintf("out of memory: the kernel OOM killer hit %s%s and systemd stopped the whole unit, this pane included", ShortUnit(s.OOM.Unit), peak(s.OOM))
		case s.OOM != nil:
			return fmt.Sprintf("probably out of memory: the OOM killer hit %s%s just before", ShortUnit(s.OOM.Unit), peak(s.OOM))
		case s.Signal == "SIGKILL":
			return "SIGKILL from outside agency, and no OOM kill in the user journal"
		case s.Signal != "":
			return "killed by " + s.Signal
		case s.Status != nil:
			return fmt.Sprintf("exited with status %d", *s.Status)
		}
		return "died"
	case KindExited:
		return "exited cleanly (status 0)"
	case OutcomeVanished:
		return "vanished with no exit hook: tmux removed it directly (kill-pane, kill-window, kill-session), or the hooks were not installed yet"
	case OutcomeHandoff:
		return "handed off to " + s.Successor + ", no death recorded"
	case KindDaemon:
		return fmt.Sprintf("agency daemon started (%s, pid %d)", s.Note, s.PID)
	case KindGuard:
		return "OOM guard installed: " + s.Note
	}
	return s.Outcome
}

func peak(oom *OOM) string {
	if oom.Peak == "" {
		return ""
	}
	return " (" + oom.Peak + " peak)"
}

// ShortUnit trims tmux-spawn scope UUIDs to their first block.
func ShortUnit(unit string) string {
	rest, ok := strings.CutPrefix(unit, "tmux-spawn-")
	if !ok || len(rest) < 8 {
		return unit
	}
	return "tmux-spawn-" + rest[:8] + "….scope"
}

// DescribeActor names who asked for a kill: a pane and its role, Lemon, or
// agency itself, plus the command that asked.
func DescribeActor(a *Actor) string {
	if a == nil {
		return "agency"
	}
	var who string
	switch {
	case a.Pane != "" && !a.Human:
		who = strings.TrimSpace(a.Pane + " " + a.Role)
	case a.Human && a.PID == 0:
		who = "Lemon (phone)"
	case a.Human:
		who = "Lemon"
	default:
		who = "agency"
	}
	if a.Command != "" {
		who += " via `" + a.Command + "`"
	}
	return who
}

// ReadSteps loads the activity diary's entries at or after since, by pane.
func ReadSteps(path string, since time.Time) map[string][]Step {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > 8<<20 {
		_, _ = f.Seek(-8<<20, io.SeekEnd)
	}
	steps := map[string][]Step{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		var entry struct {
			At   int64  `json:"at"`
			Pane string `json:"pane"`
			Kind string `json:"kind"`
			Text string `json:"text"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Pane == "" {
			continue
		}
		at := time.UnixMilli(entry.At)
		if at.Before(since) {
			continue
		}
		steps[entry.Pane] = append(steps[entry.Pane], Step{At: at, Kind: entry.Kind, Text: entry.Text})
	}
	return steps
}

// AttachSteps gives every ended story its pane's last diary entry before the end.
func AttachSteps(stories []Story, steps map[string][]Step) {
	for i := range stories {
		s := &stories[i]
		if s.Pane == "" {
			continue
		}
		for _, step := range steps[s.Pane] {
			if step.At.After(s.At) {
				break
			}
			copied := step
			s.LastStep = &copied
		}
	}
}
