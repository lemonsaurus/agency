package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/coalesce"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/live"
	"github.com/lemonsaurus/agency/internal/notify"
	"github.com/lemonsaurus/agency/internal/session"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// runCloudWatch runs on the host: it prints a line whenever the panes a sky
// harness mirrors change, a "reminder {json}" line when a reminder comes due
// while the link is up, and a "lease {json}" line at start and whenever the
// lease or its holder's focus changes. Each "shown <device> <focused>
// <keys...>" line on stdin says which of that device's viewers are on screen
// and when it last took focus: the rest are parked, windows fit the device
// focused last, and a "views" line reports the result. "claim" and "take"
// lines move the lease. It exits when the SSH client closes stdin.
func runCloudWatch(cfg *config.Config) {
	tc := tmux.NewClient(cfg.Session.Name, "")
	ctx, cancel := context.WithCancel(context.Background())
	stdin := make(chan string)
	go func() {
		defer cancel()
		lines := bufio.NewScanner(os.Stdin)
		for lines.Scan() {
			select {
			case stdin <- lines.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	last := ""
	var lease tmux.Lease
	leased := false // a lease line went out
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
		if current, err := tc.Lease(ctx); err == nil && (!leased || current != lease) {
			leased = true
			lease = current
			payload, _ := json.Marshal(lease)
			if _, err := fmt.Printf("lease %s\n", payload); err != nil {
				return
			}
		}
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
		case line := <-stdin:
			if device, focused, keys, ok := cloud.ParseShownLine(line); ok {
				devices[device] = true
				state, err := tc.ApplyShown(ctx, device, focused, keys)
				go fitOnce(tc)
				if !report(device, state, err) {
					return
				}
			}
			if verb, device, host, ok := cloud.ParseLeaseRequest(line); ok {
				take := tc.ClaimLease
				if verb == "take" {
					take = tc.TakeLease
				}
				if err := take(ctx, device, host, time.Now()); err != nil {
					log.Printf("lease %s %s: %v", verb, device, err)
				}
				go fitOnce(tc)
			}
		case <-time.After(time.Second):
		}
	}
}

// fitOnce sizes the host's windows, one fit at a time across every process:
// hooks fire on each client event, and a burst of them shares one trailing
// fit.
func fitOnce(tc *tmux.Client) error {
	_, err := coalesce.Run(filepath.Join(os.TempDir(), "agency-fit-"+os.Getenv("AGENCY_TMUX_SOCKET")), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return tc.Fit(ctx)
	})
	return err
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

// newCloudLink is this device's link to the host.
func newCloudLink(session string) *cloud.Link {
	return cloud.NewLink(cloud.Device(session), cloud.Hostname())
}

// watchCloud keeps the sky harness in step with the host over the one link
// every viewer of this device shares: every change the host reports syncs,
// the lease decides whether this device streams, and a dropped link
// reconnects with capped, jittered backoff until it gives up and waits for a
// manual retry from the sky screen.
func watchCloud(ctx context.Context, mgr *session.Manager, link *cloud.Link, host string) {
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
	// A sync collapses the sky harness to the sky screen, or brings the
	// viewers back, whenever the lease or the give-up flips.
	go func() {
		collapsed := link.Collapsed()
		for {
			changed := link.Changed()
			if now := link.Collapsed(); now != collapsed {
				collapsed = now
				trigger()
			}
			select {
			case <-ctx.Done():
				return
			case <-changed:
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
		Up:       func() { link.Up(time.Now()) },
		Changed:  trigger,
		Reminder: func(payload string) { showReminder(ctx, payload) },
		Views:    func(payload string) { log.Printf("sky views: %s", payload) },
		Lease: func(payload string) {
			var lease tmux.Lease
			if err := json.Unmarshal([]byte(payload), &lease); err == nil {
				link.SetLease(lease)
			}
		},
	}
	for {
		link.Connecting()
		// A fresh process or a fresh link has told the host nothing yet.
		if err := mgr.ResendVisibility(ctx); err != nil {
			log.Printf("cloud watch: visibility: %v", err)
		}
		err := remote.Watch(ctx, handlers, shown, link.Requests())
		if ctx.Err() != nil {
			return
		}
		delay, offline := link.Dropped(err, time.Now(), cloud.Jitter())
		var wait <-chan time.Time
		if offline {
			log.Printf("cloud watch: link closed (%v); gave up after %d tries until a manual retry", err, cloud.LinkAttempts)
		} else {
			log.Printf("cloud watch: link closed (%v); reconnecting in %s", err, delay.Round(time.Millisecond))
			wait = time.After(delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-wait:
		case <-link.Retried():
		}
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
