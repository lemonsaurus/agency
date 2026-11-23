package cloud

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/tmux"
)

func TestBackoffDoublesToAMinuteWithJitter(t *testing.T) {
	for failed, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 6: 32 * time.Second, 7: time.Minute, 40: time.Minute} {
		if got := Backoff(failed, 0); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", failed, got, want)
		}
	}
	if got := Backoff(7, 0.999); got <= 30*time.Second || got >= time.Minute {
		t.Errorf("jitter shaved %s off a minute", time.Minute-got)
	}
}

func TestLinkGivesUpAfterLinkAttempts(t *testing.T) {
	link := NewLink("bluefin-agency", "bluefin")
	now := time.Now()
	for i := 1; i < LinkAttempts; i++ {
		link.Connecting()
		delay, offline := link.Dropped(errors.New("refused"), now, 0)
		if offline || delay != Backoff(i, 0) {
			t.Fatalf("failure %d: delay %s offline %v", i, delay, offline)
		}
	}
	if link.Collapsed() {
		t.Fatal("retrying link collapsed the sky harness")
	}
	if _, offline := link.Dropped(errors.New("refused"), now, 0); !offline || !link.Collapsed() {
		t.Fatal("link kept retrying past LinkAttempts")
	}
	if status := link.Status(); status.State != LinkOffline || status.Error != "refused" {
		t.Fatalf("status = %+v", status)
	}
	link.Retry()
	select {
	case <-link.Retried():
	default:
		t.Fatal("manual retry did not wake the watch")
	}
	link.Connecting()
	if delay, offline := link.Dropped(nil, now, 0); offline || delay != time.Second {
		t.Fatalf("manual retry kept the old count: %s %v", delay, offline)
	}
}

func TestLinkCountsFlappingButForgivesASteadyLink(t *testing.T) {
	link := NewLink("bluefin-agency", "bluefin")
	now := time.Now()
	link.Up(now)
	link.Dropped(nil, now.Add(time.Second), 0)
	link.Up(now)
	if delay, _ := link.Dropped(nil, now.Add(time.Second), 0); delay != 2*time.Second {
		t.Fatalf("a link that dropped at once reset its count: %s", delay)
	}
	link.Up(now)
	if delay, _ := link.Dropped(nil, now.Add(2*time.Minute), 0); delay != time.Second {
		t.Fatalf("a steady link kept its count: %s", delay)
	}
}

func TestLinkLeaseClaimsTakesAndCollapses(t *testing.T) {
	link := NewLink("bluefin-agency", "bluefin")
	link.Up(time.Now())
	link.SetLease(tmux.Lease{})
	if got := <-link.Requests(); got != "claim bluefin-agency bluefin" {
		t.Fatalf("unheld lease request = %q", got)
	}
	link.SetLease(tmux.Lease{Device: "CITRUS-BRUTE-agency", Host: "CITRUS-BRUTE"})
	if !link.Collapsed() || !link.Elsewhere() || link.Status().Here {
		t.Fatal("lease elsewhere did not collapse the sky harness")
	}
	select {
	case got := <-link.Requests():
		t.Fatalf("held lease sent %q: a device never takes over on its own", got)
	default:
	}
	link.Take()
	if got := <-link.Requests(); got != "take bluefin-agency bluefin" {
		t.Fatalf("take request = %q", got)
	}
	link.SetLease(tmux.Lease{Device: "bluefin-agency", Host: "bluefin"})
	if link.Collapsed() {
		t.Fatal("holding the lease left the sky harness collapsed")
	}
}

func TestLinkWaitHoldsViewersUntilUpAndPacesHiddenOnes(t *testing.T) {
	saved := attachPace
	attachPace = 40 * time.Millisecond
	t.Cleanup(func() { attachPace = saved })
	link := NewLink("bluefin-agency", "bluefin")
	link.SetWindows([]string{"@1", "@2", "@3"})
	ctx := context.Background()
	if gone, err := link.Wait(ctx, "@9", true); !gone || err != nil {
		t.Fatalf("missing window: gone=%v err=%v", gone, err)
	}

	released := make(chan time.Time, 3)
	for _, window := range []string{"@1", "@2", "@3"} {
		go func() {
			if gone, err := link.Wait(ctx, window, false); gone || err != nil {
				t.Errorf("%s: gone=%v err=%v", window, gone, err)
			}
			released <- time.Now()
		}()
	}
	select {
	case <-released:
		t.Fatal("a viewer attached before the link came up")
	case <-time.After(50 * time.Millisecond):
	}
	start := time.Now()
	link.Up(start)
	var last time.Time
	for range 3 {
		last = <-released
	}
	if spread := last.Sub(start); spread < 2*attachPace {
		t.Fatalf("three hidden viewers attached within %s, want paced by %s", spread, attachPace)
	}
	shown := time.Now()
	if _, err := link.Wait(ctx, "@1", true); err != nil || time.Since(shown) > attachPace/2 {
		t.Fatalf("a viewer on screen waited %s behind the pace", time.Since(shown))
	}

	link.SetLease(tmux.Lease{Device: "CITRUS-BRUTE-agency", Host: "CITRUS-BRUTE"})
	waiting, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := link.Wait(waiting, "@1", true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("viewer attached while another device held the lease: %v", err)
	}
}
