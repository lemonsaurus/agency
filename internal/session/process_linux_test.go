//go:build linux

package session

import (
	"os"
	"testing"
)

func TestProcessDescendsFrom(t *testing.T) {
	if !processDescendsFrom(os.Getpid(), os.Getppid()) {
		t.Fatal("current process should descend from its parent")
	}
	if processDescendsFrom(os.Getppid(), os.Getpid()) {
		t.Fatal("parent process should not descend from its child")
	}
}
