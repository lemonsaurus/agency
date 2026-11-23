package cloud

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/lemonsaurus/agency/internal/tmux"
)

// LinkState is where a device's watch link to the host stands.
type LinkState string

const (
	LinkConnecting LinkState = "connecting" // a connect is under way
	LinkUp         LinkState = "up"         // the host answered on the watch link
	LinkRetrying   LinkState = "retrying"   // the link dropped; the next connect waits for its backoff
	LinkOffline    LinkState = "offline"    // connects failed LinkAttempts times; only a manual retry reconnects
)

// LinkAttempts is how many connects in a row may fail before the link goes
// offline and waits for a manual retry.
const LinkAttempts = 10

// linkSteady is how long a link must stay up before its drop no longer counts
// toward LinkAttempts. A host that accepts and drops at once still gives up.
const linkSteady = time.Minute

// attachPace spaces the attaches of viewers that are off screen, so a device
// that reconnects never hands the host its whole mirror at once. Viewers on
// screen skip the queue.
var attachPace = 50 * time.Millisecond

// Backoff is the pause after the given failed connect, counting from 1:
// doubling from a second, capped at a minute, less up to half of it by jitter
// in [0, 1), so devices never reconnect in step.
func Backoff(failed int, jitter float64) time.Duration {
	delay := time.Minute
	if failed < 7 {
		delay = min(delay, time.Second<<max(failed-1, 0))
	}
	return delay - time.Duration(float64(delay)*jitter/2)
}

// Link is one device's connection to the host. The watch keeps it up, every
// viewer waits on it before it attaches, and the sky screen shows it when the
// device does not stream: the lease is elsewhere or the link gave up.
type Link struct {
	device, host string

	mu       sync.Mutex
	state    LinkState
	failed   int       // connects failed in a row
	upAt     time.Time // when the link last came up
	retryAt  time.Time // when the next connect starts while retrying
	err      string    // why the link last dropped
	lease    tmux.Lease
	windows  map[string]bool // remote windows at the last sync; nil before one
	slot     time.Time       // when the next off-screen viewer may attach
	changed  chan struct{}   // closed and replaced on every change
	retry    chan struct{}
	requests chan string
}

// NewLink starts a link for device on the machine named host.
func NewLink(device, host string) *Link {
	return &Link{
		device:   device,
		host:     host,
		state:    LinkConnecting,
		changed:  make(chan struct{}),
		retry:    make(chan struct{}, 1),
		requests: make(chan string, 4),
	}
}

// notify wakes everyone waiting on a change. Call it with mu held.
func (l *Link) notify() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// Changed is closed on the next change.
func (l *Link) Changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.changed
}

// Requests carries claim and take lines for the host.
func (l *Link) Requests() <-chan string {
	return l.requests
}

// Retried fires when someone asks an offline or retrying link to connect now.
func (l *Link) Retried() <-chan struct{} {
	return l.retry
}

// Connecting marks a connect under way.
func (l *Link) Connecting() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state = LinkConnecting
	l.notify()
}

// Up marks the link up: the host answered.
func (l *Link) Up(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state, l.upAt, l.err = LinkUp, now, ""
	l.notify()
}

// Dropped records a failed connect or a dropped link and returns the pause
// before the next connect. A link that stayed up past linkSteady starts its
// count over; offline reports that the count ran out.
func (l *Link) Dropped(err error, now time.Time, jitter float64) (delay time.Duration, offline bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == LinkUp && now.Sub(l.upAt) >= linkSteady {
		l.failed = 0
	}
	l.failed++
	if err != nil {
		l.err = err.Error()
	}
	defer l.notify()
	if l.failed >= LinkAttempts {
		l.state = LinkOffline
		return 0, true
	}
	delay = Backoff(l.failed, jitter)
	l.state, l.retryAt = LinkRetrying, now.Add(delay)
	return delay, false
}

// Retry asks for a connect now, and gives an offline link a fresh count.
func (l *Link) Retry() {
	l.mu.Lock()
	if l.state == LinkOffline {
		l.failed = 0
	}
	l.mu.Unlock()
	select {
	case l.retry <- struct{}{}:
	default:
	}
}

// SetLease records the host's lease and claims it when nobody holds it.
func (l *Link) SetLease(lease tmux.Lease) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lease = lease
	if lease.Device == "" {
		l.request(LeaseRequest("claim", l.device, l.host))
	}
	l.notify()
}

// Take asks the host to move the lease here.
func (l *Link) Take() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.request(LeaseRequest("take", l.device, l.host))
}

func (l *Link) request(line string) {
	select {
	case l.requests <- line:
	default:
	}
}

// Elsewhere reports whether another device holds the lease.
func (l *Link) Elsewhere() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.lease.Holds(l.device)
}

// Collapsed reports whether the sky harness shows only the sky screen: the
// lease is elsewhere or the link gave up. A collapsed device runs no viewers.
func (l *Link) Collapsed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state == LinkOffline || !l.lease.Holds(l.device)
}

// SetWindows records the remote windows the last sync saw.
func (l *Link) SetWindows(ids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.windows = make(map[string]bool, len(ids))
	for _, id := range ids {
		l.windows[id] = true
	}
}

// Wait blocks a viewer until it may attach: the link is up and this device
// holds the lease. Off-screen viewers then take the next paced slot. gone
// reports that the last sync no longer saw the window.
func (l *Link) Wait(ctx context.Context, window string, shown bool) (gone bool, err error) {
	for {
		l.mu.Lock()
		if l.windows != nil && !l.windows[window] {
			l.mu.Unlock()
			return true, nil
		}
		if l.state == LinkUp && l.lease.Holds(l.device) {
			now := time.Now()
			at := now
			if !shown {
				at = l.slot
				if at.Before(now) {
					at = now
				}
				l.slot = at.Add(attachPace)
			}
			l.mu.Unlock()
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(at.Sub(now)):
				return false, nil
			}
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-changed:
		}
	}
}

// LinkStatus is what the sky screen draws.
type LinkStatus struct {
	State    LinkState  `json:"state"`
	Failed   int        `json:"failed,omitempty"`
	Attempts int        `json:"attempts"`
	RetryAt  int64      `json:"retryAt,omitempty"` // Unix ms
	Error    string     `json:"error,omitempty"`
	Lease    tmux.Lease `json:"lease"`
	Here     bool       `json:"here"`    // this device holds the lease, or nobody does
	Windows  int        `json:"windows"` // remote windows at the last sync, -1 before one
}

// Status snapshots the link.
func (l *Link) Status() LinkStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	status := LinkStatus{State: l.state, Failed: l.failed, Attempts: LinkAttempts, Error: l.err, Lease: l.lease, Here: l.lease.Holds(l.device), Windows: -1}
	if l.state == LinkRetrying {
		status.RetryAt = l.retryAt.UnixMilli()
	}
	if l.windows != nil {
		status.Windows = len(l.windows)
	}
	return status
}

// Jitter is a random backoff jitter in [0, 1).
func Jitter() float64 {
	return rand.Float64()
}
