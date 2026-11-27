package restore

import (
	"context"
	"fmt"
	"time"

	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/manifest"
)

// Target is the session being rebuilt.
type Target interface {
	// Spawn starts one pane and returns its new ID.
	Spawn(ctx context.Context, spec control.RestoreSpec) (string, error)
	// Alive is whether the pane with this ID is the restored one for old.
	Alive(ctx context.Context, id, old string) bool
	// Notify delivers text to a pane's Pi once Pi is ready for it.
	Notify(ctx context.Context, id, text string) error
}

// Kind is what an Event reports.
type Kind string

const (
	Starting Kind = "starting" // a pane is about to start
	Started  Kind = "started"  // a pane is up
	Kept     Kind = "kept"     // a pane from an earlier run is still up
	Failed   Kind = "failed"   // a pane did not start
	Telling  Kind = "telling"  // a recovery note is being delivered
	Told     Kind = "told"     // a recovery note arrived
	Unsent   Kind = "unsent"   // a recovery note did not arrive
)

// Event is one step of progress.
type Event struct {
	Kind  Kind
	Step  Step
	New   string
	Err   error
	Index int // 1-based position in the run
	Total int
}

// Result is what a run did.
type Result struct {
	Restored map[string]string
	Failed   map[string]error
	Unsent   map[string]error
}

// Ok is whether every pane is back and told.
func (r Result) Ok() bool { return len(r.Failed) == 0 && len(r.Unsent) == 0 }

// Pace is the wait after each started pane, so a box does not start every Pi
// at once.
var Pace = 1500 * time.Millisecond

// noteTimeout bounds the wait for one Pi to come up and take its note.
var noteTimeout = 2 * time.Minute

// Run restores steps into target. pending carries what an earlier run
// already did; save persists it after every pane, so a run that stops
// resumes from there. Panes start one at a time, parents first.
func Run(ctx context.Context, target Target, m manifest.Manifest, steps []Step, pending *manifest.Pending, save func(manifest.Pending) error, emit func(Event)) Result {
	if pending.Restored == nil {
		pending.Restored = map[string]string{}
	}
	result := Result{Restored: pending.Restored, Failed: map[string]error{}, Unsent: map[string]error{}}
	var todo []Step
	for _, step := range steps {
		if step.Skip == "" {
			todo = append(todo, step)
		}
	}
	total := len(todo)
	persist := func() {
		if save != nil {
			_ = save(*pending)
		}
	}
	for i, step := range todo {
		event := Event{Step: step, Index: i + 1, Total: total}
		if ctx.Err() != nil {
			result.Failed[step.Pane.ID] = ctx.Err()
			continue
		}
		if id, ok := pending.Restored[step.Pane.ID]; ok && target.Alive(ctx, id, step.Pane.ID) {
			event.Kind, event.New = Kept, id
			emit(event)
			continue
		}
		if parent := step.Pane.Parent; parent != "" {
			if err := result.Failed[parent]; err != nil {
				result.Failed[step.Pane.ID] = fmt.Errorf("parent %s did not come back", parent)
				event.Kind, event.Err = Failed, result.Failed[step.Pane.ID]
				emit(event)
				continue
			}
		}
		event.Kind = Starting
		emit(event)
		id, err := target.Spawn(ctx, step.Spec(pending.Restored))
		if err != nil {
			result.Failed[step.Pane.ID] = err
			event.Kind, event.Err = Failed, err
			emit(event)
			continue
		}
		pending.Restored[step.Pane.ID] = id
		persist()
		event.Kind, event.New = Started, id
		emit(event)
		if i < total-1 {
			select {
			case <-ctx.Done():
			case <-time.After(Pace):
			}
		}
	}
	for i, step := range todo {
		id, ok := pending.Restored[step.Pane.ID]
		if !ok || !step.Resumed || pending.Noted[step.Pane.ID] {
			continue
		}
		event := Event{Step: step, New: id, Index: i + 1, Total: total}
		event.Kind = Telling
		emit(event)
		noteCtx, cancel := context.WithTimeout(ctx, noteTimeout)
		err := target.Notify(noteCtx, id, Note(m, step.Pane.ID, pending.Restored))
		cancel()
		if err != nil {
			result.Unsent[step.Pane.ID] = err
			event.Kind, event.Err = Unsent, err
			emit(event)
			continue
		}
		if pending.Noted == nil {
			pending.Noted = map[string]bool{}
		}
		pending.Noted[step.Pane.ID] = true
		persist()
		event.Kind = Told
		emit(event)
	}
	return result
}
