package restore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/manifest"
)

func sample() manifest.Manifest {
	return manifest.Manifest{Windows: []manifest.Window{
		{Name: "control", Panes: []manifest.Pane{{ID: "%0", Role: "controller", Command: "zsh"}}},
		{Name: "kid", Group: "beta", Panes: []manifest.Pane{{ID: "%9", Role: "worker", Parent: "%5", Root: "%1", Label: "Worker", Command: "pi", Session: "/s/w.jsonl"}}},
		{Name: "boss", Group: "alpha", Panes: []manifest.Pane{{ID: "%1", Role: "controller", Root: "%1", Label: "Boss", Command: "pi", Session: "/s/b.jsonl"}}},
		{Name: "mid", Group: "gamma", Panes: []manifest.Pane{{ID: "%5", Role: "manager", Parent: "%1", Root: "%1", Label: "Mid", Command: "pi --model x --session /old.jsonl", Session: "/s/m with space.jsonl"}}},
		{Name: "dev", Group: "runtimes", Panes: []manifest.Pane{{ID: "%7", Role: "worker", Parent: "%1", Root: "%1", Label: "Dev server", Command: "bash /tmp/dev.sh"}}},
		{Name: "retired", Group: "x", Panes: []manifest.Pane{{ID: "%4", Role: "manager", Label: "Mother Superior", Command: "pi"}}},
	}}
}

// Parents come back before children, the control window and skipped panes
// stay dead, Pi panes resume their conversation and plain commands rerun.
func TestPlanOrdersParentsFirstAndResumesPi(t *testing.T) {
	steps := Plan(sample(), Options{Skip: []string{"mother"}})
	var order []string
	for _, step := range steps {
		if step.Skip == "" {
			order = append(order, step.Pane.ID)
		}
	}
	if got := strings.Join(order, " "); got != "%1 %5 %7 %9" {
		t.Fatalf("order = %s", got)
	}
	for _, step := range steps {
		switch step.Pane.ID {
		case "%0":
			if step.Skip != "control window" {
				t.Fatalf("control pane skip = %q", step.Skip)
			}
		case "%4":
			if step.Skip != "skipped" {
				t.Fatalf("skipped pane skip = %q", step.Skip)
			}
		case "%5":
			if step.Command != "pi --model x --session '/s/m with space.jsonl'" || !step.Resumed || step.Window != "gamma" {
				t.Fatalf("mid = %+v", step)
			}
		case "%7":
			if step.Command != "bash /tmp/dev.sh" || step.Resumed {
				t.Fatalf("dev = %+v", step)
			}
		}
	}
}

func TestSessionFromCommand(t *testing.T) {
	for command, want := range map[string]string{"pi --session /a/b.jsonl": "/a/b.jsonl", "pi --session=/a/c.jsonl --x": "/a/c.jsonl", "pi": ""} {
		if got := commandSession(command); got != want {
			t.Errorf("commandSession(%q) = %q, want %q", command, got, want)
		}
	}
}

type fakeTarget struct {
	next    int
	specs   []control.RestoreSpec
	alive   map[string]bool
	failOn  string
	told    map[string]string
	noteErr error
}

func (f *fakeTarget) Spawn(_ context.Context, spec control.RestoreSpec) (string, error) {
	if spec.Old == f.failOn {
		return "", errors.New("pane limit reached")
	}
	f.next++
	id := fmt.Sprintf("%%%d", 100+f.next)
	f.specs = append(f.specs, spec)
	f.alive[id] = true
	return id, nil
}
func (f *fakeTarget) Alive(_ context.Context, id, _ string) bool { return f.alive[id] }
func (f *fakeTarget) Notify(_ context.Context, id, text string) error {
	if f.noteErr != nil {
		return f.noteErr
	}
	f.told[id] = text
	return nil
}

// Every restored pane gets its parent's and root's new IDs, each resumed Pi
// is told its old and new IDs and its relatives', and a second run does
// nothing.
func TestRunRestoresInOrderTellsPiAndIsIdempotent(t *testing.T) {
	Pace = 0
	m := sample()
	steps := Plan(m, Options{Skip: []string{"%4"}})
	target := &fakeTarget{alive: map[string]bool{}, told: map[string]string{}}
	pending := &manifest.Pending{Manifest: m}
	saves := 0
	result := Run(context.Background(), target, m, steps, pending, func(manifest.Pending) error { saves++; return nil }, func(Event) {})
	if !result.Ok() || len(result.Restored) != 4 {
		t.Fatalf("result = %+v", result)
	}
	mid := target.specs[1]
	if mid.Old != "%5" || mid.Parent != "%101" || mid.Root != "%101" || mid.Role != control.RoleManager || mid.Window != "gamma" || mid.Label != "Mid" {
		t.Fatalf("mid spec = %+v", mid)
	}
	note := target.told["%102"]
	for _, want := range []string{"You were %5; you are now %102", "Your parent is now %101 (Boss)", "%9 -> %104 (Worker)", "check before starting duplicates"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q: %s", want, note)
		}
	}
	if len(target.told) != 3 || saves < 4 {
		t.Fatalf("told %d panes, saved %d times", len(target.told), saves)
	}
	spawned := len(target.specs)
	again := Run(context.Background(), target, m, steps, pending, nil, func(Event) {})
	if len(target.specs) != spawned || len(target.told) != 3 || !again.Ok() {
		t.Fatalf("a second run started %d panes and told %d", len(target.specs)-spawned, len(target.told))
	}
}

// A pane that will not start is reported, its children wait for the next
// run, and a pane Pi never took the note for stays unsent.
func TestRunReportsFailuresAndSkipsOrphans(t *testing.T) {
	Pace = 0
	noteTimeout = time.Second
	m := sample()
	target := &fakeTarget{alive: map[string]bool{}, told: map[string]string{}, failOn: "%5", noteErr: errors.New("no bridge")}
	result := Run(context.Background(), target, m, Plan(m, Options{Skip: []string{"%4"}}), &manifest.Pending{Manifest: m}, nil, func(Event) {})
	if result.Ok() || result.Failed["%5"] == nil || result.Failed["%9"] == nil || result.Failed["%7"] != nil {
		t.Fatalf("failed = %v", result.Failed)
	}
	if len(result.Unsent) != 1 || result.Unsent["%1"] == nil {
		t.Fatalf("unsent = %v", result.Unsent)
	}
}

func TestSessionFileNameCarriesItsCreationTime(t *testing.T) {
	created, ok := nameTime("2026-11-27T09-47-46-911Z_01a21c67-615e-761b-963c-88cc36a731a0.jsonl")
	if !ok || !created.Equal(time.Date(2026, 11, 27, 9, 47, 46, 911_000_000, time.UTC)) {
		t.Fatalf("created = %v, %v", created, ok)
	}
	if _, ok := nameTime("notes.jsonl"); ok {
		t.Fatal("a stray file parsed as a session")
	}
}
