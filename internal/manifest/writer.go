package manifest

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/lemonsaurus/agency/internal/tmux"
)

// Source is the tmux server the writer records.
type Source interface {
	ListPanes(ctx context.Context) ([]tmux.PaneInfo, error)
	ServerID(ctx context.Context) (string, error)
}

// Writer keeps the manifest file current. Poke after a change; a slow tick
// catches whatever nothing poked for. It writes nothing while a restore is
// pending, so the dead session's manifest survives until it is rebuilt.
type Writer struct {
	Path        string
	PendingPath string
	Session     string
	Source      Source
	SessionFile func(paneID, command string) string

	poke chan struct{}
	mu   sync.Mutex
	last Manifest
}

// settle is how long a burst of changes waits to land in one write.
const settle = 150 * time.Millisecond

// tick is how often the writer looks without a poke.
const tick = 5 * time.Second

// Poke asks for a write soon. It never blocks.
func (w *Writer) Poke() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.poke == nil {
		w.poke = make(chan struct{}, 1)
	}
	ch := w.poke
	w.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Run writes until ctx ends.
func (w *Writer) Run(ctx context.Context) {
	w.mu.Lock()
	if w.poke == nil {
		w.poke = make(chan struct{}, 1)
	}
	ch := w.poke
	w.mu.Unlock()
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-ch:
			select {
			case <-ctx.Done():
				return
			case <-time.After(settle):
			}
		}
		if err := w.Write(ctx); err != nil {
			log.Printf("manifest: %v", err)
		}
	}
}

// Write records the session now. It does nothing while a restore is pending
// or the server cannot be read.
func (w *Writer) Write(ctx context.Context) error {
	if _, err := os.Stat(w.PendingPath); err == nil {
		return nil
	}
	panes, err := w.Source.ListPanes(ctx)
	if err != nil {
		return nil
	}
	server, err := w.Source.ServerID(ctx)
	if err != nil {
		return nil
	}
	next := Build(w.Session, panes, w.SessionFile)
	next.Server = server
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last.Version != 0 && Same(w.last, next) {
		return nil
	}
	next.SavedAt = time.Now().UTC()
	if err := Save(w.Path, next); err != nil {
		return err
	}
	w.last = next
	return nil
}

// Freeze starts a pending restore from the saved manifest when the saved
// server is not the one running now, and writes it unless dry. It returns the
// pending restore and whether there is one to run.
func Freeze(path, pendingPath, server string, dry bool) (Pending, bool, error) {
	if pending, ok, err := LoadPending(pendingPath); err != nil || ok {
		return pending, ok, err
	}
	saved, err := Load(path)
	if err != nil {
		return Pending{}, false, err
	}
	if saved.Server == "" || saved.Server == server || len(saved.Panes()) == 0 {
		return Pending{}, false, nil
	}
	pending := Pending{Manifest: saved, Restored: map[string]string{}, Started: time.Now().UTC()}
	if !dry {
		if err := Save(pendingPath, pending); err != nil {
			return Pending{}, false, err
		}
	}
	return pending, true, nil
}

// Finish ends a pending restore so the writer records the session again. The
// saved manifest is marked as belonging to server first, so nothing mistakes
// the rebuilt session for a dead one before the writer catches up.
func Finish(path, pendingPath, server string) error {
	if saved, err := Load(path); err == nil && saved.Server != "" && server != "" {
		saved.Server = server
		if err := Save(path, saved); err != nil {
			return err
		}
	}
	if err := os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Lock takes the exclusive right to run a restore; the returned func gives it
// back. A second restore finds it taken.
func Lock(pendingPath string) (func(), error) {
	file, err := os.OpenFile(pendingPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("a restore is already running")
	}
	return func() { syscall.Flock(int(file.Fd()), syscall.LOCK_UN); file.Close() }, nil
}
