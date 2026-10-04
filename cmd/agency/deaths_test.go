package main

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestPaneExitEventReadsTheDeadPane(t *testing.T) {
	command := base64.RawStdEncoding.EncodeToString([]byte("pi --model anthropic/claude-opus-5-5:high"))
	line := "3317461\t\t9\t/home/lemon/git/brood/.worktrees/fs-competitors\tFagstige Competitor Check\tmanager\tfagstige\tπ pi@fs-competitors\t" + command + "\texport AGENCY_ROLE=manager; pi"
	event := paneExitEvent("%1313", line)
	if event.Kind != "died" || event.Pane != "%1313" || event.PID != 3317461 || event.Status != nil || event.Signal != "SIGKILL" {
		t.Fatalf("event = %+v", event)
	}
	if event.Label != "Fagstige Competitor Check" || event.Role != "manager" || event.Group != "fagstige" || event.Command != "pi --model anthropic/claude-opus-5-5:high" {
		t.Fatalf("event = %+v", event)
	}

	exited := paneExitEvent("%4", "51\t3\t\t/tmp\t\t\t\tshells\t\tbash")
	if exited.Status == nil || *exited.Status != 3 || exited.Signal != "" || exited.Group != "shells" || exited.Command != "bash" {
		t.Fatalf("exited = %+v", exited)
	}
	if bare := paneExitEvent("%5", ""); bare.Pane != "%5" || bare.PID != 0 {
		t.Fatalf("bare = %+v", bare)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Time{
		"90m": now.Add(-90 * time.Minute),
		"48h": now.Add(-48 * time.Hour),
		"7d":  now.AddDate(0, 0, -7),
		"all": {},
	} {
		got, err := parseSince(value, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v", value, got, err)
		}
	}
	if _, err := parseSince("soon", now); err == nil {
		t.Error("parseSince accepted soon")
	}
}
