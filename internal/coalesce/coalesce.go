// Package coalesce runs an operation one at a time across processes, folding
// every request that arrives during a run into a single trailing run.
package coalesce

import (
	"errors"
	"os"
	"syscall"
)

// Run runs fn unless another caller already waits to run it. A caller that
// arrives while fn runs waits and runs once more after it; every caller after
// that one returns at once, since the waiting run covers it. A burst of any
// size costs at most two runs. ran reports whether this caller ran fn.
func Run(path string, fn func() error) (ran bool, err error) {
	queue, err := lock(path+".queue", syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer queue.Close()
	run, err := lock(path+".run", syscall.LOCK_EX)
	if err != nil {
		return false, err
	}
	defer run.Close()
	queue.Close()
	return true, fn()
}

// lock opens path and flocks it; closing the file releases the lock.
func lock(path string, how int) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
