package retry

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

var errTransient = errors.New("transient")

func instant(t *testing.T) {
	t.Helper()
	saved := Delays
	Delays = []time.Duration{0, 0}
	t.Cleanup(func() { Delays = saved })
}

func TestDoRetriesTransientFailures(t *testing.T) {
	instant(t)
	calls := 0
	var waits [][2]int
	err := Do(context.Background(), func(err error) bool { return errors.Is(err, errTransient) }, func(attempt, attempts int, err error) {
		waits = append(waits, [2]int{attempt, attempts})
	}, func() error {
		calls++
		if calls < 3 {
			return errTransient
		}
		return nil
	})
	if err != nil || calls != 3 || !reflect.DeepEqual(waits, [][2]int{{2, 3}, {3, 3}}) {
		t.Fatalf("err=%v calls=%d waits=%v", err, calls, waits)
	}
}

func TestDoStops(t *testing.T) {
	instant(t)
	transient := func(err error) bool { return errors.Is(err, errTransient) }
	permanent := errors.New("permanent")
	calls := 0
	if err := Do(context.Background(), transient, nil, func() error { calls++; return permanent }); err != permanent || calls != 1 {
		t.Fatalf("permanent: err=%v calls=%d", err, calls)
	}
	calls = 0
	if err := Do(context.Background(), transient, nil, func() error { calls++; return errTransient }); err != errTransient || calls != 3 {
		t.Fatalf("exhausted: err=%v calls=%d", err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	if err := Do(ctx, transient, nil, func() error { calls++; return errTransient }); err != errTransient || calls != 1 {
		t.Fatalf("cancelled: err=%v calls=%d", err, calls)
	}
}
