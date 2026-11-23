package budget

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lemonsaurus/agency/internal/control"
)

// Limits is where the budget says no. The shares set the ceiling on
// projected use. Workers stop Reserve short of it, so controllers and
// managers always find room, and a controller's or manager's spawn that has
// waited MaxWait goes in past the projection. The backstop, read straight
// off the box, refuses whatever the projection says.
type Limits struct {
	CPUShare        float64       `json:"cpuShare"`        // ceiling as a share of cores
	MemShare        float64       `json:"memShare"`        // ceiling as a share of RAM
	Reserve         Cost          `json:"reserve"`         // kept free of workers' spawns and jobs
	MaxWait         time.Duration `json:"maxWait"`         // longest a controller's or manager's spawn waits on the projection
	JobCost         Cost          `json:"jobCost"`         // a job's estimate before its class has run three times
	MinAvailable    uint64        `json:"minAvailable"`    // MemAvailable below this refuses
	MaxMemorySome60 float64       `json:"maxMemorySome60"` // memory PSI some avg60 above this refuses
	MaxMemoryFull10 float64       `json:"maxMemoryFull10"` // memory PSI full avg10 above this refuses
	MaxLoadPerCore  float64       `json:"maxLoadPerCore"`  // load1 per core above this refuses
}

// DefaultLimits overcommits CPU further than memory, since too little CPU
// only slows panes while too little memory kills them, and sits below the
// Harness Janitor's intervention lines (MemAvailable 6 GB, memory PSI full
// avg60 20). Disk is the Janitor's: a spawn barely writes, so a full disk
// queues nothing.
var DefaultLimits = Limits{
	CPUShare:        0.85,
	MemShare:        0.7,
	Reserve:         Cost{CPU: 1.5, Mem: 3 * GB},
	MaxWait:         10 * time.Minute,
	JobCost:         Cost{CPU: 1, Mem: 1536 << 20},
	MinAvailable:    12 * GB,
	MaxMemorySome60: 10,
	MaxMemoryFull10: 5,
	MaxLoadPerCore:  2,
}

// DefaultPaneCost is the estimate for a pane class the learner has not seen
// three times yet.
var DefaultPaneCost = Cost{CPU: 0.5, Mem: GB}

const (
	interval   = 5 * time.Second
	busyAlpha  = 0.28             // 15 s time constant at five-second samples
	usedAlpha  = 0.4              // smooths one pane or job's CPU between samples
	paneRamp   = 3 * time.Minute  // a new pane holds its estimate this long
	spawnGrace = 2 * time.Minute  // an admitted spawn holds its estimate until its pane shows up
	leaseTTL   = 30 * time.Second // a job lease without a heartbeat this long is freed
)

// Kind is what an entry in the budget is.
type Kind string

const (
	KindPane  Kind = "pane"
	KindSpawn Kind = "spawn" // admitted, pane not up yet
	KindJob   Kind = "job"
)

// Pane is one pane the daemon tracks, as the sampler needs it.
type Pane struct {
	ID       string
	PID      int
	Role     control.Role
	Class    string
	Label    string
	Window   string
	CWD      string
	Activity time.Time
}

// Entry is something running that the budget charges for.
type Entry struct {
	ID       string       `json:"id"` // pane id, spawn or job ticket
	Kind     Kind         `json:"kind"`
	Class    string       `json:"class"`
	Label    string       `json:"label,omitempty"`
	Role     control.Role `json:"role,omitempty"`
	Pane     string       `json:"pane,omitempty"` // a job's owner pane
	Window   string       `json:"window,omitempty"`
	CWD      string       `json:"cwd,omitempty"`
	PID      int          `json:"pid,omitempty"`
	Started  time.Time    `json:"started"`
	Activity time.Time    `json:"activity,omitempty"`
	Estimate Cost         `json:"estimate"`
	Used     Cost         `json:"used"`
	Peak     Cost         `json:"peak"`
	Pending  Cost         `json:"pending"` // estimate not yet showing in Used, charged on top of the box
	Procs    int          `json:"procs"`

	ticks    uint64
	measured bool    // Used.CPU has a first reading
	cpuSum   float64 // core-seconds since start, for a job's average
	lastSeen time.Time
}

// Ticket is a spawn or job waiting for budget.
type Ticket struct {
	ID       string       `json:"id"`
	Kind     Kind         `json:"kind"`
	Class    string       `json:"class"`
	Label    string       `json:"label,omitempty"`
	Role     control.Role `json:"role,omitempty"`
	Human    bool         `json:"human,omitempty"`
	Pane     string       `json:"pane,omitempty"` // requester
	PID      int          `json:"pid,omitempty"`  // a job's client process
	Cost     Cost         `json:"cost"`
	Enqueued time.Time    `json:"enqueued"`
	Payload  string       `json:"payload,omitempty"` // a spawn's request, to replay it after a restart

	start    func() (string, error)
	lastPoll time.Time
}

// Decision is the budget's answer to one request.
type Decision struct {
	Admitted  bool   `json:"admitted"`
	Ticket    string `json:"ticket"`
	Position  int    `json:"position,omitempty"` // 1-based place in the queue when not admitted
	Waiting   int    `json:"waiting,omitempty"`
	Reason    string `json:"reason,omitempty"`  // why it waits
	Warning   string `json:"warning,omitempty"` // a human spawn over budget
	Cost      Cost   `json:"cost"`
	Projected Cost   `json:"projected"`
	Ceiling   Cost   `json:"ceiling"`
}

// Snapshot is the whole budget at one moment.
type Snapshot struct {
	At        time.Time `json:"at"`
	Limits    Limits    `json:"limits"`
	Box       Box       `json:"box"`
	Busy      float64   `json:"busy"` // smoothed busy cores
	Ceiling   Cost      `json:"ceiling"`
	Committed Cost      `json:"committed"` // box use plus every pending estimate
	Headroom  Cost      `json:"headroom"`
	Backstop  string    `json:"backstop,omitempty"` // the backstop limit tripped, if any
	Entries   []Entry   `json:"entries"`
	Queue     []Ticket  `json:"queue"`
	Other     Cost      `json:"other"` // box use no pane or job accounts for
}

// Started reports a queued spawn that left the queue.
type Started struct {
	Ticket Ticket
	PaneID string
	Err    error
}

// Budget measures, learns, admits, and queues.
type Budget struct {
	mu      sync.Mutex
	limits  Limits
	learner *Learner
	box     Box
	busy    float64
	at      time.Time // when box was read
	entries map[string]*Entry
	queue   []*Ticket
	seq     int
	sampled bool

	// Panes lists the daemon's panes for each sample.
	Panes func(ctx context.Context) []Pane
	// OnStart hears when a queued spawn starts or fails, to tell its requester.
	OnStart func(Started)
}

// New creates a budget that keeps what it learns in stateDir.
func New(limits Limits, stateDir string) *Budget {
	path := ""
	if stateDir != "" {
		path = filepath.Join(stateDir, "costs.json")
	}
	return &Budget{
		limits:  limits,
		learner: newLearner(path),
		entries: map[string]*Entry{},
	}
}

// Run samples the box every five seconds and starts what fits until ctx ends.
func (b *Budget) Run(ctx context.Context) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Busy cores and CPU use are differences, so the first reading takes two samples.
	b.Sample(ctx)
	time.Sleep(time.Second)
	b.Sample(ctx)
	for {
		select {
		case <-ctx.Done():
			b.learner.save(true)
			return
		case <-ticker.C:
			b.Sample(ctx)
		}
	}
}

// Sample reads the box and every pane and job once, learns, and pumps the queue.
func (b *Budget) Sample(ctx context.Context) {
	var panes []Pane
	if b.Panes != nil {
		panes = b.Panes(ctx)
	}
	box := readBox(b.box)
	table := readProcs()

	b.mu.Lock()
	now := table.at
	if box.BusyCores > 0 {
		if b.busy == 0 {
			b.busy = box.BusyCores
		} else {
			b.busy += busyAlpha * (box.BusyCores - b.busy)
		}
	}
	elapsed := now.Sub(b.at).Seconds()
	b.box, b.at, b.sampled = box, now, true

	jobPIDs := map[int]bool{}
	for _, e := range b.entries {
		if e.Kind == KindJob && e.PID > 0 {
			jobPIDs[e.PID] = true
		}
	}
	seen := map[string]bool{}
	for _, pane := range panes {
		seen[pane.ID] = true
		e := b.entries[pane.ID]
		if e == nil {
			e = &Entry{ID: pane.ID, Kind: KindPane, Started: table.started(pane.PID, now)}
			b.entries[pane.ID] = e
		}
		e.Class, e.Label, e.Role, e.PID = pane.Class, pane.Label, pane.Role, pane.PID
		e.Window, e.CWD, e.Activity = pane.Window, pane.CWD, pane.Activity
		e.Estimate = b.learner.estimate(e.Class, DefaultPaneCost)
		b.measure(e, table, table.tree(pane.PID, jobPIDs), elapsed)
		if now.Sub(e.Started) > paneRamp {
			b.learner.observe(e.Class, e.Used, paneAlpha)
		}
	}
	for id, e := range b.entries {
		switch e.Kind {
		case KindPane:
			if !seen[id] {
				delete(b.entries, id)
			}
		case KindSpawn:
			if now.Sub(e.Started) > spawnGrace {
				delete(b.entries, id)
			}
		case KindJob:
			if now.Sub(e.lastSeen) > leaseTTL {
				b.finishJob(e)
			} else {
				b.measure(e, table, table.tree(e.PID, nil), elapsed)
			}
		}
	}
	for _, e := range b.entries {
		e.Pending = b.pending(e, now)
	}
	b.dropAbandoned(now, seen)
	started := b.pump(now)
	b.mu.Unlock()

	b.learner.save(false)
	b.launch(started)
}

func (b *Budget) measure(e *Entry, table procTable, pids []int, elapsed float64) {
	ticks, rss := table.usage(pids)
	e.Procs = len(pids)
	e.Used.Mem = rss
	if e.ticks > 0 && ticks >= e.ticks && elapsed > 0 {
		seconds := float64(ticks-e.ticks) / clockTicks
		if e.measured {
			e.Used.CPU += usedAlpha * (seconds/elapsed - e.Used.CPU)
		} else {
			e.Used.CPU, e.measured = seconds/elapsed, true
		}
		e.cpuSum += seconds
	}
	e.ticks = ticks
	e.Peak.CPU = math.Max(e.Peak.CPU, e.Used.CPU)
	e.Peak.Mem = max(e.Peak.Mem, e.Used.Mem)
}

// pending is the part of e's estimate the box does not show yet: all of an
// admitted spawn, a new pane's ramp, and a job's climb to its learned cost.
func (b *Budget) pending(e *Entry, now time.Time) Cost {
	switch e.Kind {
	case KindSpawn:
		return e.Estimate
	case KindPane:
		if now.Sub(e.Started) < paneRamp {
			return e.Estimate.over(e.Used)
		}
	case KindJob:
		return e.Estimate.over(e.Used)
	}
	return Cost{}
}

// ceiling is the share of the box the budget hands out.
func (b *Budget) ceiling() Cost {
	return Cost{
		CPU: float64(b.box.Cores) * b.limits.CPUShare,
		Mem: uint64(float64(b.box.MemTotal) * b.limits.MemShare),
	}
}

// ceilingFor is the ceiling t may fill: workers and unknown requesters stop
// the reserve short of it.
func (b *Budget) ceilingFor(t *Ticket) Cost {
	ceiling := b.ceiling()
	if rank(t) < rank(&Ticket{Role: control.RoleWorker}) {
		return ceiling
	}
	return ceiling.over(b.limits.Reserve)
}

// overdue reports whether t has waited out MaxWait as a controller's or
// manager's spawn, which then needs only the backstop's say.
func (b *Budget) overdue(t *Ticket, now time.Time) bool {
	return t.Kind == KindSpawn && rank(t) <= rank(&Ticket{Role: control.RoleManager}) &&
		b.limits.MaxWait > 0 && now.Sub(t.Enqueued) >= b.limits.MaxWait && b.backstop() == ""
}

// SetLimits replaces the limits; queued tickets are judged by them from the
// next sample.
func (b *Budget) SetLimits(limits Limits) {
	b.mu.Lock()
	b.limits = limits
	b.mu.Unlock()
}

// committed is what the box uses now plus every estimate still on its way.
func (b *Budget) committed() Cost {
	total := Cost{CPU: b.busy, Mem: b.box.MemUsed()}
	for _, e := range b.entries {
		total = total.add(e.Pending)
	}
	return total
}

// backstop names the limit the box is past right now, or "".
func (b *Budget) backstop() string {
	box, l := b.box, b.limits
	switch {
	case !b.sampled:
		return ""
	case box.MemAvailable < l.MinAvailable:
		return fmt.Sprintf("only %s memory available (floor %s)", FormatBytes(box.MemAvailable), FormatBytes(l.MinAvailable))
	case box.Memory.SomeAvg60 > l.MaxMemorySome60:
		return fmt.Sprintf("memory pressure %.0f%% over the last minute (limit %.0f%%)", box.Memory.SomeAvg60, l.MaxMemorySome60)
	case box.Memory.FullAvg10 > l.MaxMemoryFull10:
		return fmt.Sprintf("memory stalls %.0f%% over 10 s (limit %.0f%%)", box.Memory.FullAvg10, l.MaxMemoryFull10)
	case box.Load1 > l.MaxLoadPerCore*float64(box.Cores):
		return fmt.Sprintf("load %.0f on %d cores (limit %.0f)", box.Load1, box.Cores, l.MaxLoadPerCore*float64(box.Cores))
	}
	return ""
}

// fit says why t does not fit right now, or "".
func (b *Budget) fit(t *Ticket) string {
	if reason := b.backstop(); reason != "" {
		return reason
	}
	cost := t.Cost
	ceiling := b.ceilingFor(t)
	projected := b.committed().add(cost)
	switch {
	case projected.CPU > ceiling.CPU:
		return fmt.Sprintf("needs %.1f cores, %.1f of %.1f free", cost.CPU, math.Max(0, ceiling.CPU-b.committed().CPU), ceiling.CPU)
	case projected.Mem > ceiling.Mem:
		free := uint64(0)
		if c := b.committed().Mem; c < ceiling.Mem {
			free = ceiling.Mem - c
		}
		return fmt.Sprintf("needs %s memory, %s of %s free", FormatBytes(cost.Mem), FormatBytes(free), FormatBytes(ceiling.Mem))
	}
	return ""
}

// clamp caps a reservation at half the ceiling, so a job that wants the
// whole box waits for half of it, never for an idle box. Whatever it uses
// beyond that shows up as box use and holds back everything after it.
func (b *Budget) clamp(cost Cost) Cost {
	if !b.sampled {
		return cost
	}
	ceiling := b.ceiling()
	return Cost{CPU: math.Min(cost.CPU, ceiling.CPU/2), Mem: min(cost.Mem, ceiling.Mem/2)}
}

func rank(t *Ticket) int {
	if t.Human {
		return 0
	}
	switch t.Role {
	case control.RoleController:
		return 1
	case control.RoleManager:
		return 2
	case control.RoleWorker:
		return 3
	}
	return 4
}

func (b *Budget) nextID(kind Kind) string {
	b.seq++
	if kind == KindJob {
		return fmt.Sprintf("j-%d", b.seq)
	}
	return fmt.Sprintf("q-%d", b.seq)
}

// enqueue places t behind everything of its rank or better.
func (b *Budget) enqueue(t *Ticket) int {
	i := sort.Search(len(b.queue), func(i int) bool { return rank(b.queue[i]) > rank(t) })
	b.queue = append(b.queue, nil)
	copy(b.queue[i+1:], b.queue[i:])
	b.queue[i] = t
	return i + 1
}

func (b *Budget) position(id string) int {
	for i, t := range b.queue {
		if t.ID == id {
			return i + 1
		}
	}
	return 0
}

// ahead reports whether anything that outranks or ties t already waits.
func (b *Budget) ahead(t *Ticket) bool {
	return len(b.queue) > 0 && rank(b.queue[0]) <= rank(t)
}

func (b *Budget) decision(t *Ticket, admitted bool, reason string) Decision {
	return Decision{
		Admitted:  admitted,
		Ticket:    t.ID,
		Position:  b.position(t.ID),
		Waiting:   len(b.queue),
		Reason:    reason,
		Cost:      t.Cost,
		Projected: b.committed(),
		Ceiling:   b.ceilingFor(t),
	}
}

// SpawnCost is the estimate for a new pane of class.
func (b *Budget) SpawnCost(class string) Cost {
	return b.learner.estimate(class, DefaultPaneCost)
}

// Spawn admits a pane spawn or queues it. start runs the spawn and returns
// the pane id; on admission the caller runs start itself, on queueing the
// budget runs it later and reports through OnStart. A human's spawn always
// runs and carries a warning when it is over budget.
func (b *Budget) Spawn(t Ticket, start func() (string, error)) Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	ticket := &t
	ticket.Kind, ticket.start = KindSpawn, start
	ticket.ID = b.nextID(KindSpawn)
	ticket.Enqueued = time.Now()
	ticket.Cost = b.clamp(ticket.Cost)
	reason := b.fit(ticket)
	if ticket.Human || reason == "" && !b.ahead(ticket) {
		b.charge(ticket)
		d := b.decision(ticket, true, "")
		if reason != "" {
			d.Warning = "over budget: " + reason
		}
		return d
	}
	if reason == "" {
		reason = "behind higher-priority requests"
	}
	b.enqueue(ticket)
	return b.decision(ticket, false, reason)
}

// Spawned turns an admitted spawn's charge into its pane once the pane exists.
func (b *Budget) Spawned(ticket, paneID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[ticket]
	if e == nil {
		return
	}
	delete(b.entries, ticket)
	if paneID == "" {
		return
	}
	e.ID, e.Kind, e.Started = paneID, KindPane, time.Now()
	b.entries[paneID] = e
}

// charge books an admitted ticket against the budget.
func (b *Budget) charge(t *Ticket) {
	now := time.Now()
	e := &Entry{
		ID: t.ID, Kind: t.Kind, Class: t.Class, Label: t.Label, Role: t.Role,
		Pane: t.Pane, PID: t.PID, Started: now, Estimate: t.Cost, lastSeen: now,
	}
	e.Pending = b.pending(e, now)
	b.entries[t.ID] = e
}

// Job asks for, or keeps polling, budget for a heavy job. The first call
// passes an empty id; later calls pass the ticket and double as heartbeats.
// A ticket the budget no longer knows, after a daemon restart, is asked for
// again from scratch.
func (b *Budget) Job(t Ticket) Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if t.ID != "" {
		if e := b.entries[t.ID]; e != nil && e.Kind == KindJob {
			e.lastSeen = now
			return b.decision(&Ticket{ID: t.ID, Cost: e.Estimate}, true, "")
		}
		for _, q := range b.queue {
			if q.ID == t.ID {
				q.lastPoll = now
				reason := b.fit(q)
				if reason == "" {
					reason = "behind higher-priority requests"
				}
				return b.decision(q, false, reason)
			}
		}
	}
	ticket := &t
	ticket.Kind, ticket.Enqueued, ticket.lastPoll = KindJob, now, now
	if ticket.ID == "" {
		ticket.ID = b.nextID(KindJob)
	}
	if ticket.Cost == (Cost{}) {
		ticket.Cost = b.learner.estimate("job:"+ticket.Class, b.limits.JobCost)
	}
	ticket.Cost = b.clamp(ticket.Cost)
	reason := b.fit(ticket)
	if reason == "" && !b.ahead(ticket) {
		b.charge(ticket)
		return b.decision(ticket, true, "")
	}
	if reason == "" {
		reason = "behind higher-priority requests"
	}
	b.enqueue(ticket)
	return b.decision(ticket, false, reason)
}

// JobDone releases a job's budget and learns what it cost.
func (b *Budget) JobDone(id string) {
	b.mu.Lock()
	if e := b.entries[id]; e != nil && e.Kind == KindJob {
		b.finishJob(e)
	}
	b.removeQueued(id)
	started := b.pump(time.Now())
	b.mu.Unlock()
	b.launch(started)
}

func (b *Budget) finishJob(e *Entry) {
	delete(b.entries, e.ID)
	seconds := time.Since(e.Started).Seconds()
	if seconds < 2*interval.Seconds() {
		return
	}
	b.learner.observe("job:"+e.Class, Cost{CPU: e.cpuSum / seconds, Mem: e.Peak.Mem}, jobAlpha)
}

// Cancel drops a queued ticket. It reports whether one was waiting.
func (b *Budget) Cancel(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.removeQueued(id)
}

func (b *Budget) removeQueued(id string) bool {
	for i, t := range b.queue {
		if t.ID == id {
			b.queue = append(b.queue[:i], b.queue[i+1:]...)
			return true
		}
	}
	return false
}

// dropAbandoned removes queued jobs whose client stopped polling and queued
// spawns whose requester pane is gone.
func (b *Budget) dropAbandoned(now time.Time, panes map[string]bool) {
	kept := b.queue[:0]
	for _, t := range b.queue {
		switch {
		case t.Kind == KindJob && now.Sub(t.lastPoll) > leaseTTL:
		case t.Kind == KindSpawn && t.Pane != "" && len(panes) > 0 && !panes[t.Pane]:
		default:
			kept = append(kept, t)
		}
	}
	b.queue = kept
}

// pump admits queued tickets from the front while they fit. Jobs become
// leases their next poll picks up; spawns are returned for launch to start
// outside the lock.
func (b *Budget) pump(now time.Time) []*Ticket {
	var started []*Ticket
	for len(b.queue) > 0 {
		t := b.queue[0]
		if b.fit(t) != "" && !b.overdue(t, now) {
			break
		}
		b.queue = b.queue[1:]
		b.charge(t)
		if t.Kind == KindSpawn {
			started = append(started, t)
		}
	}
	return started
}

func (b *Budget) launch(tickets []*Ticket) {
	for _, t := range tickets {
		go func(t *Ticket) {
			var paneID string
			err := fmt.Errorf("spawn %s cannot be replayed", t.ID)
			if t.start != nil {
				paneID, err = t.start()
			}
			b.Spawned(t.ID, paneID)
			if b.OnStart != nil {
				b.OnStart(Started{Ticket: *t, PaneID: paneID, Err: err})
			}
		}(t)
	}
}

// Queued returns the waiting spawns, for saving across a restart.
func (b *Budget) Queued() []Ticket {
	b.mu.Lock()
	defer b.mu.Unlock()
	var spawns []Ticket
	for _, t := range b.queue {
		if t.Kind == KindSpawn {
			spawns = append(spawns, *t)
		}
	}
	return spawns
}

// Restore queues spawns saved before a restart, each with its start rebuilt.
func (b *Budget) Restore(tickets []Ticket, start func(Ticket) func() (string, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range tickets {
		ticket := t
		ticket.start = start(t)
		if n := ticketNumber(t.ID); n > b.seq {
			b.seq = n
		}
		b.enqueue(&ticket)
	}
}

func ticketNumber(id string) int {
	var n int
	if _, err := fmt.Sscanf(id[strings.IndexByte(id, '-')+1:], "%d", &n); err != nil {
		return 0
	}
	return n
}

// Snapshot returns the budget as it stands.
func (b *Budget) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Snapshot{
		At:        time.Now(),
		Limits:    b.limits,
		Box:       b.box,
		Busy:      b.busy,
		Ceiling:   b.ceiling(),
		Committed: b.committed(),
		Backstop:  b.backstop(),
	}
	s.Headroom = s.Ceiling.over(s.Committed)
	accounted := Cost{}
	for _, e := range b.entries {
		s.Entries = append(s.Entries, *e)
		if e.Kind != KindSpawn {
			accounted = accounted.add(e.Used)
		}
	}
	s.Other = Cost{CPU: b.busy, Mem: b.box.MemUsed()}.over(accounted)
	sort.Slice(s.Entries, func(i, j int) bool { return s.Entries[i].Used.Mem > s.Entries[j].Used.Mem })
	for _, t := range b.queue {
		s.Queue = append(s.Queue, *t)
	}
	return s
}
