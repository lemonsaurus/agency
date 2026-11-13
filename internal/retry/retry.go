// Package retry repeats an operation through transient failures.
package retry

import (
	"context"
	"time"
)

// Delays are the pauses before the second and third attempts.
var Delays = []time.Duration{time.Second, 3 * time.Second}

// Do runs op until it succeeds, fails with an error transient rejects, the
// attempts run out, or ctx ends. waiting hears each retry, and the failure behind it, before its
// pause.
func Do(ctx context.Context, transient func(error) bool, waiting func(attempt, attempts int, err error), op func() error) error {
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || attempt > len(Delays) || !transient(err) || ctx.Err() != nil {
			return err
		}
		if waiting != nil {
			waiting(attempt+1, len(Delays)+1, err)
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(Delays[attempt-1]):
		}
	}
}
