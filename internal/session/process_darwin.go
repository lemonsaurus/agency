//go:build darwin

package session

import (
	"os/exec"
	"strconv"
	"strings"
)

// processLineage returns pid and every ancestor up to launchd.
func processLineage(pid int) map[int]bool {
	lineage := map[int]bool{}
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return lineage
	}
	parents := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		child, err1 := strconv.Atoi(fields[0])
		parent, err2 := strconv.Atoi(fields[1])
		if err1 == nil && err2 == nil {
			parents[child] = parent
		}
	}
	for pid > 1 && !lineage[pid] {
		lineage[pid] = true
		pid = parents[pid]
	}
	return lineage
}
