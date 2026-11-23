package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeTmux(t *testing.T, script string) *ExecCommander {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return &ExecCommander{TmuxBin: path}
}

func TestRunTimesOutAndReleasesSlot(t *testing.T) {
	saved := commandTimeout
	commandTimeout = 50 * time.Millisecond
	t.Cleanup(func() { commandTimeout = saved })
	c := fakeTmux(t, "exec sleep 10\n")
	if _, err := c.Run(context.Background(), "list-panes"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if len(commandSlots) != 0 {
		t.Fatalf("slots in use = %d", len(commandSlots))
	}
}

func TestRunWaitForSlotRespectsContext(t *testing.T) {
	for i := 0; i < cap(commandSlots); i++ {
		commandSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < cap(commandSlots); i++ {
			<-commandSlots
		}
	})
	marker := filepath.Join(t.TempDir(), "started")
	c := fakeTmux(t, "touch '"+marker+"'\n")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Run(ctx, "list-panes"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command ran without a slot: %v", err)
	}
}

func TestRunReleasesSlotOnExit(t *testing.T) {
	c := fakeTmux(t, "printf listed\n")
	if out, err := c.Run(context.Background(), "list-panes"); err != nil || out != "listed" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	c = fakeTmux(t, "echo refused >&2; exit 1\n")
	if _, err := c.Run(context.Background(), "list-panes"); err == nil {
		t.Fatal("expected command failure")
	}
	if len(commandSlots) != 0 {
		t.Fatalf("slots in use = %d", len(commandSlots))
	}
}
