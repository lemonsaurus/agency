package cloud

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/tmux"
)

var escape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// plain is a frame's text without escapes.
func plain(frame string) string {
	return escape.ReplaceAllString(frame, "")
}

func TestScreenSaysWhereTheSkyHarnessIsInUse(t *testing.T) {
	now := time.Now()
	status := LinkStatus{State: LinkUp, Attempts: LinkAttempts, Lease: tmux.Lease{Device: "bluefin-agency", Host: "bluefin", Taken: now.Add(-2 * time.Hour).UnixMilli(), Active: now.Add(-4 * time.Minute).UnixMilli()}}
	frame := Screen(status, "sky", "", "", now, 120, 40)
	if !strings.Contains(frame, "In use on \x1b[0m"+fg(screenMauve)+"\x1b[1mbluefin") {
		t.Error("hostname is not in the accent")
	}
	frame = plain(frame)
	for _, want := range []string{"In use on bluefin", "taken 2 h ago", "active 4 min ago", "Enter takes over here", "╭", "╯"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame lacks %q", want)
		}
	}
	if !strings.HasPrefix(Screen(status, "sky", "", "", now, 120, 40), "\x1b[?25l\x1b[0m\x1b[2J") {
		t.Error("frame does not clear the terminal first")
	}
	status.State = LinkRetrying
	if frame := Screen(status, "sky", "", "", now, 120, 40); !strings.Contains(frame, "Enter reconnects") {
		t.Error("in-use screen with a dropped link offers a takeover")
	}
}

func TestScreenStates(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		status LinkStatus
		want   []string
	}{
		{LinkStatus{State: LinkOffline, Attempts: 10, Here: true, Error: "ssh: connect to host sky: Connection timed out"}, []string{"Can't reach sky", "gave up after 10 tries", "Enter retries", "Connection timed out"}},
		{LinkStatus{State: LinkRetrying, Failed: 2, Attempts: 10, Here: true, RetryAt: now.Add(12 * time.Second).UnixMilli()}, []string{"Connecting to sky", "try 3 of 10", "next in 12s"}},
		{LinkStatus{State: LinkUp, Attempts: 10, Here: true}, []string{"No remote panes on sky", "Prefix+t", "Enter re-syncs"}},
	} {
		frame := plain(Screen(tc.status, "sky", "Prefix+t opens a shell there", "", now, 100, 30))
		for _, want := range tc.want {
			if !strings.Contains(frame, want) {
				t.Errorf("%s frame lacks %q", tc.status.State, want)
			}
		}
	}
	if frame := Screen(LinkStatus{State: LinkUp, Here: true}, "sky", "", "⟳  taking over", now, 100, 30); !strings.Contains(frame, "taking over") || strings.Contains(frame, "Enter re-syncs") {
		t.Error("note did not replace the action")
	}
}

func TestScreenFitsTinyTerminals(t *testing.T) {
	status := LinkStatus{State: LinkUp, Lease: tmux.Lease{Device: "x", Host: "a-very-long-hostname-that-will-not-fit"}}
	for _, size := range [][2]int{{1, 1}, {10, 3}, {30, 8}, {200, 60}} {
		frame := Screen(status, "sky", "", "", time.Now(), size[0], size[1])
		if frame == "" {
			t.Fatalf("%v drew nothing", size)
		}
	}
	if Screen(status, "sky", "", "", time.Now(), 0, 10) != "" {
		t.Fatal("drew into a zero-width terminal")
	}
}

func TestAgo(t *testing.T) {
	now := time.Now()
	for d, want := range map[time.Duration]string{3 * time.Second: "just now", 40 * time.Second: "40s ago", 5 * time.Minute: "5 min ago", 3 * time.Hour: "3 h ago", 72 * time.Hour: "3 days ago"} {
		if got := Ago(now, now.Add(-d).UnixMilli()); got != want {
			t.Errorf("Ago(%s) = %q, want %q", d, got, want)
		}
	}
}
