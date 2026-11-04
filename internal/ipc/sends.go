package ipc

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

// ResendWindow is how long an agent's identical send to the same pane counts
// as a retry of the first rather than a new message.
const ResendWindow = 2 * time.Minute

// AlreadySent prefixes the reply to a retry the ledger absorbed, followed by
// the first delivery's RFC 3339 time.
const AlreadySent = "already-sent "

// sendLedger makes agent sends idempotent. A caller that gives up on a slow
// send and retries joins the delivery still in flight, or learns it already
// arrived, instead of delivering the message a second time.
type sendLedger struct {
	mu      sync.Mutex
	entries map[[32]byte]*sentMessage
}

type sentMessage struct {
	done chan struct{}
	err  error
	at   time.Time // when the delivery finished
}

func sendKey(from, to, text string, enter bool) [32]byte {
	return sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%t\x00%s", from, to, enter, text))
}

// deliver runs send unless the same message is in flight or arrived within
// ResendWindow. It returns when that delivery arrived, or zero after running
// send itself. A failed delivery is forgotten, so its retry sends again.
func (l *sendLedger) deliver(ctx context.Context, key [32]byte, send func() error) (time.Time, error) {
	for {
		l.mu.Lock()
		now := time.Now()
		for k, sent := range l.entries {
			if !sent.at.IsZero() && now.Sub(sent.at) > ResendWindow {
				delete(l.entries, k)
			}
		}
		sent := l.entries[key]
		if sent == nil {
			sent = &sentMessage{done: make(chan struct{})}
			if l.entries == nil {
				l.entries = map[[32]byte]*sentMessage{}
			}
			l.entries[key] = sent
			l.mu.Unlock()
			err := send()
			l.mu.Lock()
			sent.err, sent.at = err, time.Now()
			if err != nil {
				delete(l.entries, key)
			}
			close(sent.done)
			l.mu.Unlock()
			return time.Time{}, err
		}
		l.mu.Unlock()
		select {
		case <-sent.done:
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		}
		if sent.err == nil {
			return sent.at, nil
		}
	}
}
