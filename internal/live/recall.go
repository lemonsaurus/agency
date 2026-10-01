package live

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Recall is short-term memory of what recent delegations fetched and answered, plus the updates
// that reached the conversation since, so a follow-up reuses results instead of asking the box again.
type Recall struct {
	mu        sync.Mutex
	exchanges []Exchange
	updates   []recalledUpdate
}

// Exchange is one delegation: what Lemon said, what the backend's tools returned, and its reply.
type Exchange struct {
	At      time.Time
	Asked   string
	Fetched []Fetched
	Said    string
	Details string
}

// Fetched is one tool call the backend made while answering.
type Fetched struct {
	Tool      string
	Arguments string
	Result    string
}

type recalledUpdate struct {
	At   time.Time
	Text string
}

const (
	recallKeep   = 5    // exchanges kept, and updates kept
	recallResult = 1500 // characters kept per tool result
	recallBrief  = 20000
)

func (r *Recall) Add(exchange Exchange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	exchange.Asked = truncate(exchange.Asked, 600)
	for i := range exchange.Fetched {
		exchange.Fetched[i].Result = truncate(exchange.Fetched[i].Result, recallResult)
	}
	r.exchanges = append(r.exchanges, exchange)
	if len(r.exchanges) > recallKeep {
		r.exchanges = r.exchanges[len(r.exchanges)-recallKeep:]
	}
}

// Note records an update that reached the conversation: session progress, a finish, a Discord message.
func (r *Recall) Note(text string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, recalledUpdate{at, truncate(text, 600)})
	if len(r.updates) > recallKeep {
		r.updates = r.updates[len(r.updates)-recallKeep:]
	}
}

// Checked is whether a delegation since t called any tool.
func (r *Recall) Checked(since time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, exchange := range r.exchanges {
		if !exchange.At.Before(since) && len(exchange.Fetched) > 0 {
			return true
		}
	}
	return false
}

// Brief lists the recent exchanges oldest first, within the size budget, then the updates that
// arrived after the oldest one listed. Empty when nothing was delegated yet.
func (r *Recall) Brief(now time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var blocks []string
	budget := recallBrief
	for i := len(r.exchanges) - 1; i >= 0; i-- {
		block := r.exchanges[i].brief(now)
		if len(block) > budget {
			break
		}
		budget -= len(block)
		blocks = append([]string{block}, blocks...)
	}
	if len(blocks) == 0 {
		return ""
	}
	oldest := r.exchanges[len(r.exchanges)-len(blocks)].At
	var b strings.Builder
	b.WriteString("Recent results, oldest first.")
	for _, block := range blocks {
		b.WriteString("\n\n" + block)
	}
	header := false
	for _, update := range r.updates {
		if update.At.Before(oldest) {
			continue
		}
		if !header {
			b.WriteString("\n\nUpdates since:")
			header = true
		}
		fmt.Fprintf(&b, "\n- %s: %s", ago(now.Sub(update.At)), update.Text)
	}
	return b.String()
}

func (e Exchange) brief(now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, Lemon said: %s", ago(now.Sub(e.At)), e.Asked)
	for _, fetched := range e.Fetched {
		tool := fetched.Tool
		if args := strings.TrimSpace(fetched.Arguments); args != "" && args != "{}" {
			tool += " " + args
		}
		fmt.Fprintf(&b, "\n%s returned: %s", tool, fetched.Result)
	}
	fmt.Fprintf(&b, "\nYou answered: %s", e.Said)
	if e.Details != "" {
		fmt.Fprintf(&b, "\nDetails: %s", e.Details)
	}
	return b.String()
}
