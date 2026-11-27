package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/ipc"
	"github.com/lemonsaurus/agency/internal/manifest"
	"github.com/lemonsaurus/agency/internal/restore"
	"github.com/lemonsaurus/agency/internal/session"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// manifestPath is the durable record of the session's panes.
func manifestPath(sessionName string) string {
	return filepath.Join(stateDir(), "manifest-"+sessionName+".json")
}

// pendingPath is the restore in progress, when there is one.
func pendingPath(sessionName string) string {
	return filepath.Join(stateDir(), "restore-"+sessionName+".json")
}

const (
	// superviseEvery is how often the daemon asks tmux whether it is there.
	superviseEvery = 3 * time.Second
	// deadChecks is how many checks in a row must find the server gone.
	deadChecks = 2
	// stuckAfter is how long tmux may go without answering before Lemon hears.
	stuckAfter = 45 * time.Second
)

// recovery rebuilds the session after its tmux server dies.
type recovery struct {
	tc     *tmux.Client
	mgr    *session.Manager
	socket string
	// tmuxSocket is the server's socket name, for the environment panes get.
	tmuxSocket string
	// alert tells Lemon's phone and desktops.
	alert func(text string)
	// setup gives a new server the daemon's environment.
	setup func(ctx context.Context)
	// restored runs when a rebuild finishes, so the manifest writer starts again.
	restored func()
}

func (r *recovery) paths() (string, string) {
	return manifestPath(r.tc.SessionName), pendingPath(r.tc.SessionName)
}

// resume finishes a restore a stopped daemon left half done, or the one a dead
// server's manifest started at launch.
func (r *recovery) resume(ctx context.Context) {
	_, pending := r.paths()
	state, ok, err := manifest.LoadPending(pending)
	if err != nil || !ok {
		return
	}
	unlock, err := manifest.Lock(pending)
	if err != nil {
		return
	}
	defer unlock()
	r.alert(fmt.Sprintf("The sky session restarted. Restoring %d panes.", startable(restore.Plan(state.Manifest, restore.Options{}))))
	r.run(ctx, state)
}

// watch checks the tmux server until ctx ends. A server that is gone twice in
// a row is replaced and its panes restored; one that stops answering is
// reported.
func (r *recovery) watch(ctx context.Context) {
	ticker := time.NewTicker(superviseEvery)
	defer ticker.Stop()
	down := 0
	var silent time.Time
	told := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		_, err := r.tc.ServerID(ctx)
		switch {
		case err == nil:
			down, silent, told = 0, time.Time{}, false
		case tmux.ServerDown(err):
			silent, told = time.Time{}, false
			if down++; down >= deadChecks {
				down = 0
				r.revive(ctx, err)
			}
		default:
			if silent.IsZero() {
				silent = time.Now()
			}
			if !told && time.Since(silent) > stuckAfter {
				told = true
				log.Printf("recovery: tmux has not answered for %s: %v", stuckAfter, err)
				r.alert("The sky tmux server is not answering. Agents keep running, but nothing can spawn or label until it is back.")
			}
		}
	}
}

// revive replaces a dead tmux server and restores the panes the manifest
// holds. cause is what the daemon saw.
func (r *recovery) revive(ctx context.Context, cause error) {
	log.Printf("recovery: tmux server is gone: %v", cause)
	path, pending := r.paths()
	unlock, err := manifest.Lock(pending)
	if err != nil {
		log.Printf("recovery: %v", err)
		return
	}
	defer unlock()
	state, ok, err := manifest.Freeze(path, pending, "", false)
	if err != nil {
		log.Printf("recovery: reading the manifest: %v", err)
	}
	panes := startable(restore.Plan(state.Manifest, restore.Options{}))
	if ok {
		r.alert(fmt.Sprintf("The sky tmux server died. Starting a new session and restoring %d panes.", panes))
	} else {
		r.alert("The sky tmux server died. Starting a new session; nothing was saved to restore.")
	}
	if err := r.rebuild(ctx); err != nil {
		log.Printf("recovery: %v", err)
		r.alert("The sky tmux server died and a new one would not start: " + err.Error())
		return
	}
	if ok {
		r.run(ctx, state)
	} else if r.restored != nil {
		r.restored()
	}
}

// rebuild starts a fresh tmux session the way startup does.
func (r *recovery) rebuild(ctx context.Context) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = r.tc.NewSession(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		return err
	}
	r.mgr.Forget()
	clearSessions(r.socket)
	if r.setup != nil {
		r.setup(ctx)
	}
	if err := r.tc.NameFirstWindow(ctx, "control"); err != nil {
		log.Printf("recovery: naming control window: %v", err)
	}
	return r.mgr.AdoptOrphans(ctx)
}

// run restores a frozen manifest into this daemon's session and tells Lemon
// how it went.
func (r *recovery) run(ctx context.Context, state manifest.Pending) {
	_, pending := r.paths()
	result := restore.Run(ctx, localTarget{r.mgr, r.tc, r.socket}, state.Manifest, restore.Plan(state.Manifest, restore.Options{}), &state,
		func(p manifest.Pending) error { return manifest.Save(pending, p) },
		func(event restore.Event) {
			switch event.Kind {
			case restore.Started:
				log.Printf("recovery: %s is now %s (%s)", event.Step.Pane.ID, event.New, event.Step.Pane.Label)
			case restore.Failed, restore.Unsent:
				log.Printf("recovery: %s %s: %v", event.Step.Pane.ID, event.Kind, event.Err)
			}
		})
	total := len(result.Restored) + len(result.Failed)
	if result.Ok() {
		server, _ := r.tc.ServerID(ctx)
		if err := manifest.Finish(manifestPath(r.tc.SessionName), pending, server); err != nil {
			log.Printf("recovery: %v", err)
		}
		r.alert(fmt.Sprintf("The sky session is back: %d of %d panes restored.", len(result.Restored), total))
		if r.restored != nil {
			r.restored()
		}
		return
	}
	r.alert(fmt.Sprintf("The sky session is partly back: %d of %d panes restored. Run `agency restore` to finish.", len(result.Restored), total))
}

// clearSessions removes the session-file notes of panes that no longer exist.
func clearSessions(socket string) {
	path, err := ipc.SessionPath(socket, "%0")
	if err != nil {
		return
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.session"))
	for _, file := range files {
		os.Remove(file)
	}
}

// localTarget restores into this process's own session.
type localTarget struct {
	mgr    *session.Manager
	tc     *tmux.Client
	socket string
}

func (t localTarget) Spawn(ctx context.Context, spec control.RestoreSpec) (string, error) {
	return t.mgr.RestorePane(ctx, spec)
}

func (t localTarget) Alive(ctx context.Context, id, old string) bool {
	return paneRestoredFrom(ctx, t.tc, id, old)
}

func (t localTarget) Notify(ctx context.Context, id, text string) error {
	return restore.BridgeNotifier(t.socket)(ctx, id, text)
}

// paneRestoredFrom is whether pane id exists and was restored from old.
func paneRestoredFrom(ctx context.Context, tc *tmux.Client, id, old string) bool {
	out, err := tc.Cmd.Run(ctx, "display-message", "-p", "-t", id, "#{@agency_restored}")
	return err == nil && strings.TrimSpace(out) == old
}

// startable counts the steps that start a pane.
func startable(steps []restore.Step) int {
	n := 0
	for _, step := range steps {
		if step.Skip == "" {
			n++
		}
	}
	return n
}
