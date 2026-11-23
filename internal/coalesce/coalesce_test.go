package coalesce

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestBurstDuringARunCostsOneTrailingRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fit")
	var runs atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	first := make(chan bool)
	go func() {
		ran, _ := Run(path, func() error {
			runs.Add(1)
			close(entered)
			<-release
			return nil
		})
		first <- ran
	}()
	<-entered

	const burst = 50
	results := make(chan bool, burst)
	var wg sync.WaitGroup
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ran, err := Run(path, func() error { runs.Add(1); return nil })
			if err != nil {
				t.Error(err)
			}
			results <- ran
		}()
	}
	for range burst - 1 {
		if <-results {
			t.Fatal("a coalesced caller ran while the first run was still going")
		}
	}
	close(release)
	wg.Wait()
	if !<-first || !<-results {
		t.Fatal("the first caller or the queued caller did not run")
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
}

func TestRunsAgainOnceIdle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fit")
	runs := 0
	for range 3 {
		if ran, err := Run(path, func() error { runs++; return nil }); !ran || err != nil {
			t.Fatalf("ran=%v err=%v", ran, err)
		}
	}
	if runs != 3 {
		t.Fatalf("runs = %d", runs)
	}
}
