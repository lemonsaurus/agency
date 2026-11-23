package budget

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/control"
)

// idleBox is a 12-core, 46 GB box doing nothing.
func idleBox() *Budget {
	b := New(DefaultLimits, "")
	b.box = Box{Cores: 12, MemTotal: 46 * GB, MemAvailable: 44 * GB}
	b.busy = 1
	b.sampled = true
	return b
}

func TestSpawnAdmittedUnderCeiling(t *testing.T) {
	b := idleBox()
	d := b.Spawn(Ticket{Role: control.RoleWorker, Cost: Cost{CPU: 1, Mem: GB}}, nil)
	if !d.Admitted || d.Ticket == "" {
		t.Fatalf("got %+v, want admitted", d)
	}
	if got := b.committed(); got.CPU != 2 {
		t.Errorf("committed %.1f cores, want the busy core plus the spawn's", got.CPU)
	}
}

func TestPendingEstimatesQueueABurst(t *testing.T) {
	b := idleBox()
	// Workers may fill 10.2 cores less the 1.5 reserve; each admitted spawn
	// holds its estimate before the box shows any of it.
	var admitted int
	for range 10 {
		if b.Spawn(Ticket{Role: control.RoleWorker, Cost: Cost{CPU: 1.5, Mem: GB}}, nil).Admitted {
			admitted++
		}
	}
	if admitted != 5 {
		t.Errorf("admitted %d of a burst of 10, want 5", admitted)
	}
	if len(b.queue) != 5 {
		t.Errorf("queued %d, want 5", len(b.queue))
	}
}

func TestQueueOrdersByRoleThenArrival(t *testing.T) {
	b := idleBox()
	b.box.MemAvailable = 2 * GB // backstop: everything waits
	b.Spawn(Ticket{Role: control.RoleWorker, Label: "w1"}, nil)
	b.Spawn(Ticket{Role: control.RoleManager, Label: "m1"}, nil)
	b.Spawn(Ticket{Role: control.RoleWorker, Label: "w2"}, nil)
	b.Spawn(Ticket{Role: control.RoleController, Label: "c1"}, nil)
	var order []string
	for _, q := range b.queue {
		order = append(order, q.Label)
	}
	if got, want := strings.Join(order, " "), "c1 m1 w1 w2"; got != want {
		t.Errorf("queue %q, want %q", got, want)
	}
}

func TestHumanSpawnBypassesWithWarning(t *testing.T) {
	b := idleBox()
	b.box.MemAvailable = 2 * GB
	d := b.Spawn(Ticket{Human: true, Role: control.RoleController}, nil)
	if !d.Admitted || !strings.Contains(d.Warning, "memory available") {
		t.Errorf("got %+v, want admitted with a memory warning", d)
	}
}

func TestBackstopRefusesWhateverTheProjection(t *testing.T) {
	b := idleBox()
	b.box.Load1 = 30
	d := b.Spawn(Ticket{Role: control.RoleManager, Cost: Cost{CPU: 0.1}}, nil)
	if d.Admitted || !strings.Contains(d.Reason, "load 30") {
		t.Errorf("got %+v, want queued for load", d)
	}
}

func TestLowerRankWaitsBehindQueuedHigherRank(t *testing.T) {
	b := idleBox()
	b.Job(Ticket{Role: control.RoleManager, Cost: Cost{CPU: 20, Mem: GB}}) // clamped to half the ceiling
	big := b.Job(Ticket{Role: control.RoleManager, Cost: Cost{CPU: 7, Mem: GB}})
	small := b.Spawn(Ticket{Role: control.RoleWorker, Cost: Cost{CPU: 0.1}}, nil)
	if big.Admitted || small.Admitted || small.Position != 2 || big.Position != 1 {
		t.Errorf("big %+v small %+v, want both queued, small second", big, small)
	}
}

func TestPumpStartsQueuedSpawnWhenRoomFrees(t *testing.T) {
	b := idleBox()
	b.busy = 3
	job := b.Job(Ticket{Role: control.RoleWorker, Cost: Cost{CPU: 4, Mem: GB}})
	started := make(chan Started, 1)
	b.OnStart = func(s Started) { started <- s }
	d := b.Spawn(Ticket{Role: control.RoleWorker, Pane: "%1", Cost: Cost{CPU: 2}}, func() (string, error) { return "%9", nil })
	if !job.Admitted || d.Admitted {
		t.Fatalf("job %+v spawn %+v, want the job running and the spawn queued", job, d)
	}
	b.JobDone(job.Ticket)
	select {
	case s := <-started:
		if s.PaneID != "%9" || s.Ticket.ID != d.Ticket {
			t.Errorf("started %+v", s)
		}
	case <-time.After(time.Second):
		t.Fatal("queued spawn never started")
	}
}

func TestJobPollKeepsItsTicket(t *testing.T) {
	b := idleBox()
	b.box.MemAvailable = 2 * GB
	first := b.Job(Ticket{Role: control.RoleWorker, Class: "cargo build"})
	again := b.Job(Ticket{ID: first.Ticket})
	if first.Admitted || again.Ticket != first.Ticket || again.Position != 1 {
		t.Errorf("first %+v again %+v", first, again)
	}
	b.box.MemAvailable = 40 * GB
	b.pump(time.Now())
	if d := b.Job(Ticket{ID: first.Ticket}); !d.Admitted {
		t.Errorf("after room freed got %+v, want admitted", d)
	}
}

func TestLearnerEstimatesAfterThreeSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "costs.json")
	l := newLearner(path)
	fallback := Cost{CPU: 9}
	for range 3 {
		if got := l.estimate("pi/worker", fallback); got != fallback {
			t.Fatalf("estimate %+v before three samples, want fallback", got)
		}
		l.observe("pi/worker", Cost{CPU: 0.4, Mem: GB}, paneAlpha)
	}
	if got := l.estimate("pi/worker", fallback); got.CPU != 0.4*margin {
		t.Errorf("estimate %+v", got)
	}
	l.save(true)
	if got := newLearner(path).estimate("pi/worker", fallback); got.Mem != uint64(GB*margin) {
		t.Errorf("reloaded estimate %+v", got)
	}
}

func TestParseCost(t *testing.T) {
	for in, want := range map[string]Cost{
		"4c,6g": {CPU: 4, Mem: 6 * GB},
		"0.5c":  {CPU: 0.5},
		"512m":  {Mem: 512 << 20},
	} {
		if got, err := ParseCost(in); err != nil || got != want {
			t.Errorf("ParseCost(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if _, err := ParseCost("4 cores"); err == nil {
		t.Error("ParseCost accepted 4 cores")
	}
}

func TestParseStatHandlesParenthesesInCommand(t *testing.T) {
	line := "42 (a) b (c)) S 7 42 42 0 -1 4194304 1 0 0 0 30 20 5 5 20 0 1 0 999 1000 25 18446744073709551615"
	p, ok := parseStat(42, []byte(line))
	if !ok || p.ppid != 7 || p.ticks != 60 || p.start != 999 || p.rss != 25*pageSize {
		t.Errorf("got %+v %v", p, ok)
	}
}

func TestReadProcsBuildsTrees(t *testing.T) {
	root := t.TempDir()
	procRoot = root
	t.Cleanup(func() { procRoot = "/proc" })
	for pid, ppid := range map[string]string{"10": "1", "11": "10", "12": "11", "13": "10"} {
		dir := filepath.Join(root, pid)
		os.MkdirAll(dir, 0o755)
		stat := pid + " (x) S " + ppid + " 0 0 0 0 0 0 0 0 0 10 0 0 0 0 0 1 0 5 0 2"
		os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644)
	}
	table := readProcs()
	if got := len(table.tree(10, nil)); got != 4 {
		t.Errorf("tree of 10 has %d processes, want 4", got)
	}
	if got := len(table.tree(10, map[int]bool{11: true})); got != 2 {
		t.Errorf("tree of 10 skipping the job at 11 has %d processes, want 2", got)
	}
	if ticks, _ := table.usage(table.tree(10, nil)); ticks != 40 {
		t.Errorf("ticks %d, want 40", ticks)
	}
}

func TestParsePressure(t *testing.T) {
	p := parsePressure("some avg10=58.80 avg60=36.26 avg300=18.16 total=1\nfull avg10=0.44 avg60=0.69 avg300=0.77 total=2\n")
	if p.SomeAvg10 != 58.8 || p.SomeAvg60 != 36.26 || p.FullAvg10 != 0.44 || p.FullAvg60 != 0.69 {
		t.Errorf("got %+v", p)
	}
}

func TestReserveKeepsRoomForManagers(t *testing.T) {
	b := idleBox()
	b.busy = 8 // ceiling 10.2 cores, workers stop at 8.7
	worker := b.Spawn(Ticket{Role: control.RoleWorker, Cost: Cost{CPU: 1}}, nil)
	manager := b.Spawn(Ticket{Role: control.RoleManager, Cost: Cost{CPU: 1}}, nil)
	if worker.Admitted || !manager.Admitted {
		t.Errorf("worker %+v manager %+v, want the manager admitted into the reserve", worker, manager)
	}
}

func TestOverdueManagerSpawnPassesTheProjection(t *testing.T) {
	b := idleBox()
	b.busy = 11
	started := make(chan Started, 1)
	b.OnStart = func(s Started) { started <- s }
	d := b.Spawn(Ticket{Role: control.RoleManager, Cost: Cost{CPU: 1}}, func() (string, error) { return "%7", nil })
	if d.Admitted {
		t.Fatalf("got %+v, want queued", d)
	}
	b.launch(b.pump(time.Now()))
	if len(b.queue) != 1 {
		t.Fatal("manager spawn left the queue before MaxWait")
	}
	b.queue[0].Enqueued = time.Now().Add(-DefaultLimits.MaxWait)
	b.launch(b.pump(time.Now()))
	select {
	case s := <-started:
		if s.PaneID != "%7" {
			t.Errorf("started %+v", s)
		}
	case <-time.After(time.Second):
		t.Fatal("overdue manager spawn never started")
	}
}

func TestOverdueWorkerAndBackstopStillWait(t *testing.T) {
	b := idleBox()
	b.busy = 11
	b.Spawn(Ticket{Role: control.RoleWorker, Cost: Cost{CPU: 1}}, nil)
	b.queue[0].Enqueued = time.Now().Add(-time.Hour)
	if started := b.pump(time.Now()); len(started) != 0 {
		t.Error("an overdue worker spawn passed the projection")
	}
	b.queue = nil
	b.Spawn(Ticket{Role: control.RoleManager, Cost: Cost{CPU: 1}}, nil)
	b.queue[0].Enqueued = time.Now().Add(-time.Hour)
	b.box.MemAvailable = 2 * GB
	if started := b.pump(time.Now()); len(started) != 0 {
		t.Error("an overdue manager spawn passed the backstop")
	}
}
