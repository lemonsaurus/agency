package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"
)

// dumpProfilesOnSignal writes the daemon's goroutine, heap, and threadcreate
// profiles, plus every goroutine's stack as text, into a new private
// directory under the temp dir on each SIGUSR1, and logs where. Profiles
// carry arguments and paths, so the directory is 0700 and the files 0600.
func dumpProfilesOnSignal(ctx context.Context) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	go func() {
		defer signal.Stop(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
			}
			dir, err := os.MkdirTemp("", "agency-profile-"+time.Now().Format("20060102-150405")+"-")
			if err != nil {
				log.Printf("profiles: %v", err)
				continue
			}
			for _, dump := range []struct {
				name, file string
				debug      int
			}{{"goroutine", "goroutine.pprof", 0}, {"goroutine", "goroutines.txt", 2}, {"heap", "heap.pprof", 0}, {"threadcreate", "threadcreate.pprof", 0}} {
				file, err := os.OpenFile(filepath.Join(dir, dump.file), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				if err != nil {
					log.Printf("profiles: %v", err)
					continue
				}
				if err := pprof.Lookup(dump.name).WriteTo(file, dump.debug); err != nil {
					log.Printf("profiles: %s: %v", dump.file, err)
				}
				file.Close()
			}
			var memory runtime.MemStats
			runtime.ReadMemStats(&memory)
			log.Printf("profiles: %s (goroutines %d, heap in use %d MiB, from OS %d MiB)", dir, runtime.NumGoroutine(), memory.HeapInuse>>20, memory.Sys>>20)
		}
	}()
}
