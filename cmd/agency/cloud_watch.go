package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/session"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// runCloudWatch runs on the host: it prints a line whenever the panes a sky
// harness mirrors change, and exits when the SSH client closes stdin.
func runCloudWatch(cfg *config.Config) {
	tc := tmux.NewClient(cfg.Session.Name, "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	last := ""
	for {
		if panes, err := tc.ListPanes(ctx); err == nil {
			if signature := watchSignature(panes, cfg.Session.Name); signature != last {
				last = signature
				if _, err := fmt.Println("changed"); err != nil {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// watchSignature covers everything sync mirrors: panes, windows, groups,
// labels, pending promotions, and folders. Viewer sessions repeat panes.
func watchSignature(panes []tmux.PaneInfo, session string) string {
	var b strings.Builder
	for _, p := range panes {
		if p.Session != session {
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.WindowID, p.Group, p.TaskLabel, p.PendingPromotion, p.CWD)
	}
	return b.String()
}

// watchCloud keeps the sky harness in step with the host: every change the
// host reports syncs, and a dropped link reconnects with backoff.
func watchCloud(ctx context.Context, mgr *session.Manager, host string) {
	syncs := make(chan struct{}, 1)
	trigger := func() {
		select {
		case syncs <- struct{}{}:
		default:
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-syncs:
				if result, err := mgr.SyncCloud(ctx); err != nil {
					log.Printf("sync-cloud: %v", err)
				} else {
					log.Printf("sync-cloud: %s", result)
				}
			}
		}
	}()
	trigger()
	remote := &cloud.Client{Host: host}
	delay := time.Second
	for {
		started := time.Now()
		err := remote.Watch(ctx, trigger)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
		}
		log.Printf("cloud watch: link closed (%v); reconnecting in %s", err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Minute)
	}
}
