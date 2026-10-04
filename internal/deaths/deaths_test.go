package deaths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordReadRoundTrip(t *testing.T) {
	l := &Log{Path: filepath.Join(t.TempDir(), "run", "pane-deaths.jsonl")}
	start := time.Now().Add(-time.Minute)
	status := 3
	l.Record(Event{Kind: KindDied, Pane: "%7", Status: &status, Label: "Pack Lane"})
	l.Record(Event{Kind: KindGone, Pane: "%7", At: start.Add(-time.Hour)})

	events, err := l.Read(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Pane != "%7" || *events[0].Status != 3 || events[0].At.IsZero() {
		t.Fatalf("events = %+v", events)
	}
}

func TestNilLogRecordsNothing(t *testing.T) {
	var l *Log
	l.Record(Event{Kind: KindDied, Pane: "%1"})
}

func TestRecordRotatesIntoOneGeneration(t *testing.T) {
	l := &Log{Path: filepath.Join(t.TempDir(), "pane-deaths.jsonl")}
	if err := os.WriteFile(l.Path, []byte(strings.Repeat("junk\n", (MaxBytes-10)/5)), 0o600); err != nil {
		t.Fatal(err)
	}
	l.Record(Event{Kind: KindExited, Pane: "%2"})

	if info, err := os.Stat(l.Path + ".1"); err != nil || info.Size() < MaxBytes-20 {
		t.Fatalf("rotated file: %v %v", info, err)
	}
	events, err := l.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Pane != "%2" {
		t.Fatalf("events = %+v", events)
	}
}

// journal is the user manager's teardown of %1313's scope on 3 October 2026,
// trimmed to the lines the probe reads.
const journal = `{"__REALTIME_TIMESTAMP":"1791064402000000","USER_UNIT":"tmux-spawn-2f5fb2c1-018a-43ae-8ee5-805826e85f2f.scope","MESSAGE":"tmux-spawn-2f5fb2c1-018a-43ae-8ee5-805826e85f2f.scope: A process of this unit has been killed by the OOM killer."}
{"__REALTIME_TIMESTAMP":"1791064406000000","USER_UNIT":"tmux-spawn-2f5fb2c1-018a-43ae-8ee5-805826e85f2f.scope","MESSAGE":"Killing process 3317461 (pi) with signal SIGKILL."}
{"__REALTIME_TIMESTAMP":"1791064406000001","USER_UNIT":"tmux-spawn-2f5fb2c1-018a-43ae-8ee5-805826e85f2f.scope","MESSAGE":"Killing process 3470591 (pnpm-native) with signal SIGKILL."}
{"__REALTIME_TIMESTAMP":"1791064406100000","USER_UNIT":"tmux-spawn-2f5fb2c1-018a-43ae-8ee5-805826e85f2f.scope","MESSAGE":"Failed with result 'oom-kill'."}
{"__REALTIME_TIMESTAMP":"1791064406200000","USER_UNIT":"tmux-spawn-2f5fb2c1-018a-43ae-8ee5-805826e85f2f.scope","MESSAGE":"Consumed 23min 24.914s CPU time, 27.7G memory peak, 0B memory swap peak."}
{"__REALTIME_TIMESTAMP":"1791064300000000","USER_UNIT":"paseo.service","MESSAGE":"relay_control_stale_terminating"}
`

func TestParseJournalMatchesThePaneProcess(t *testing.T) {
	oom := parseJournal(strings.NewReader(journal), 3317461, time.UnixMicro(1791064406300000))
	want := OOM{Unit: "tmux-spawn-2f5fb2c1-018a-43ae-8ee5-805826e85f2f.scope", Peak: "27.7G", Exact: true}
	if oom == nil || *oom != want {
		t.Fatalf("oom = %+v", oom)
	}
}

func TestParseJournalFallsBackToANearbyOOMKill(t *testing.T) {
	oom := parseJournal(strings.NewReader(journal), 99, time.UnixMicro(1791064410000000))
	if oom == nil || oom.Exact || oom.Peak != "27.7G" {
		t.Fatalf("oom = %+v", oom)
	}
	if late := parseJournal(strings.NewReader(journal), 99, time.UnixMicro(1791064402000000).Add(2*time.Minute)); late != nil {
		t.Fatalf("an OOM kill two minutes earlier still matched: %+v", late)
	}
}

func TestStoriesMergeOneDeath(t *testing.T) {
	at := time.Date(2026, 10, 3, 21, 53, 26, 0, time.UTC)
	stories := Stories([]Event{
		{At: at.Add(2 * time.Second), Kind: KindGone, Pane: "%1313", Label: "Fagstige Competitor Check", Role: "manager"},
		{At: at, Kind: KindDied, Pane: "%1313", PID: 3317461, Signal: "SIGKILL", OOM: &OOM{Unit: "tmux-spawn-2f5fb2c1-018a.scope", Peak: "27.7G", Exact: true}},
	})
	if len(stories) != 1 {
		t.Fatalf("stories = %+v", stories)
	}
	s := stories[0]
	if s.Outcome != KindDied || !s.At.Equal(at) || s.Label != "Fagstige Competitor Check" || s.PID != 3317461 || len(s.Events) != 2 {
		t.Fatalf("story = %+v", s)
	}
	if !strings.HasPrefix(s.Cause, "out of memory: the kernel OOM killer hit tmux-spawn-2f5fb2c1….scope (27.7G peak)") {
		t.Fatalf("cause = %q", s.Cause)
	}
}

func TestStoriesFollowAHandoffToTheKill(t *testing.T) {
	at := time.Date(2026, 10, 3, 22, 34, 0, 0, time.UTC)
	stories := Stories([]Event{
		{At: at, Kind: KindHandoff, Pane: "%1319", Successor: "%1329", Label: "YRK Packs"},
		{At: at.Add(2 * time.Hour), Kind: KindKilled, Pane: "%1319", By: &Actor{Pane: "%1329", Role: "manager", PID: 4242, Command: "agency kill %1319"}},
	})
	if len(stories) != 1 {
		t.Fatalf("stories = %+v", stories)
	}
	s := stories[0]
	if s.Outcome != KindKilled || s.Successor != "%1329" || !s.At.Equal(at.Add(2*time.Hour)) {
		t.Fatalf("story = %+v", s)
	}
	if s.Cause != "killed by %1329 manager via `agency kill %1319`" {
		t.Fatalf("cause = %q", s.Cause)
	}
}

func TestStoriesSeparateReusedPaneIDs(t *testing.T) {
	at := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	stories := Stories([]Event{
		{At: at, Kind: KindGone, Pane: "%4"},
		{At: at.Add(time.Hour), Kind: KindExited, Pane: "%4"},
		{At: at.Add(30 * time.Minute), Kind: KindDaemon, PID: 7, Note: "headless"},
	})
	if len(stories) != 3 || stories[0].Outcome != OutcomeVanished || stories[1].Outcome != KindDaemon || stories[2].Outcome != KindExited {
		t.Fatalf("stories = %+v", stories)
	}
	if !strings.HasPrefix(stories[0].Cause, "vanished with no exit hook") {
		t.Fatalf("cause = %q", stories[0].Cause)
	}
}

func TestAttachStepsTakesTheLastStepBeforeTheEnd(t *testing.T) {
	at := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	stories := []Story{{Pane: "%9", At: at}}
	AttachSteps(stories, map[string][]Step{"%9": {
		{At: at.Add(-time.Minute), Text: "bash pnpm test"},
		{At: at.Add(time.Minute), Text: "after"},
	}})
	if stories[0].LastStep == nil || stories[0].LastStep.Text != "bash pnpm test" {
		t.Fatalf("step = %+v", stories[0].LastStep)
	}
}

func TestDescribeActor(t *testing.T) {
	cases := map[string]*Actor{
		"agency":                           nil,
		"Lemon (phone)":                    {Human: true, Role: "controller"},
		"Lemon via `agency kill %3`":       {Human: true, PID: 5, Command: "agency kill %3"},
		"%12 worker via `agency kill %12`": {Pane: "%12", Role: "worker", PID: 6, Command: "agency kill %12"},
	}
	for want, actor := range cases {
		if got := DescribeActor(actor); got != want {
			t.Errorf("DescribeActor(%+v) = %q, want %q", actor, got, want)
		}
	}
}

func TestParseMeminfoAndSignals(t *testing.T) {
	if got := parseMeminfo("MemTotal:       48318608 kB\nMemFree:  100 kB\nMemAvailable:    1258291 kB\n"); got != "1.2G of 46.1G free" {
		t.Fatalf("memory = %q", got)
	}
	if SignalName("9") != "SIGKILL" || SignalName("") != "" || SignalName("31") != "signal 31" {
		t.Fatal("signal names")
	}
}
