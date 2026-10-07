package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/live"
	"github.com/lemonsaurus/agency/internal/notify"
	"github.com/lemonsaurus/agency/internal/session"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// runCloudWatch runs on the host: it prints a line whenever the panes a sky
// harness mirrors change, and a "reminder {json}" line when a reminder comes
// due while the link is up. Each "shown <device> <keys...>" line on stdin
// says which of that device's viewers are on screen: the rest are parked, and
// a "views" line reports the result. It exits when the SSH client closes
// stdin.
func runCloudWatch(cfg *config.Config) {
	tc := tmux.NewClient(cfg.Session.Name, "")
	ctx, cancel := context.WithCancel(context.Background())
	shown := make(chan string)
	go func() {
		defer cancel()
		lines := bufio.NewScanner(os.Stdin)
		for lines.Scan() {
			select {
			case shown <- lines.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	last := ""
	since := time.Now()
	devices := map[string]bool{}
	report := func(device string, state tmux.ViewState, err error) bool {
		if err != nil {
			log.Printf("views %s: %v", device, err)
			return true
		}
		_, werr := fmt.Printf("views %s shown=%d/%d moved=%d\n", device, state.Shown, state.Views, state.Moved)
		return werr == nil
	}
	for tick := 0; ; tick++ {
		now := time.Now()
		for _, reminder := range live.DueReminders(remindersPath(), since, now) {
			payload, _ := json.Marshal(reminder)
			if _, err := fmt.Printf("reminder %s\n", payload); err != nil {
				return
			}
		}
		since = now
		if panes, err := tc.ListPanes(ctx); err == nil {
			if signature := watchSignature(panes, cfg.Session.Name); signature != last {
				last = signature
				if _, err := fmt.Println("changed"); err != nil {
					return
				}
			}
		}
		if tick%10 == 0 {
			tc.SweepViews(ctx)
		}
		for device := range devices {
			if state, err := tc.Reconcile(ctx, device); err == nil && state.Moved > 0 {
				if !report(device, state, nil) {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case line := <-shown:
			if device, keys, ok := cloud.ParseShownLine(line); ok {
				devices[device] = true
				state, err := tc.ApplyShown(ctx, device, keys)
				if !report(device, state, err) {
					return
				}
			}
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
	// The channel holds only the latest shown set, so a link that is down
	// never queues stale ones.
	shown := make(chan string, 1)
	send := func(line string) {
		select {
		case <-shown:
		default:
		}
		select {
		case shown <- line:
		default:
		}
	}
	mgr.SetShownSink(send)
	trigger()
	remote := &cloud.Client{Host: host}
	handlers := cloud.WatchHandlers{
		Changed:  trigger,
		Reminder: func(payload string) { showReminder(ctx, payload) },
		Views:    func(payload string) { log.Printf("sky views: %s", payload) },
	}
	delay := time.Second
	for {
		started := time.Now()
		if line := mgr.ShownLine(); line != "" {
			send(line)
		}
		err := remote.Watch(ctx, handlers, shown)
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

// showReminder puts a reminder from the host on this desktop.
func showReminder(ctx context.Context, payload string) {
	var reminder live.Reminder
	if json.Unmarshal([]byte(payload), &reminder) != nil {
		return
	}
	if err := notify.Show(ctx, "Reminder", reminder.Text); err != nil {
		log.Printf("reminder: notify: %v", err)
	}
}
