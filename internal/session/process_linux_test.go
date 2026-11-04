//go:build linux

package session

import (
	"os"
	"testing"
)

func TestProcessLineage(t *testing.T) {
	lineage := processLineage(os.Getpid())
	if !lineage[os.Getpid()] || !lineage[os.Getppid()] {
		t.Fatalf("lineage %v should hold the process and its parent", lineage)
	}
	if processLineage(os.Getppid())[os.Getpid()] {
		t.Fatal("a parent's lineage should not hold its child")
	}
}
