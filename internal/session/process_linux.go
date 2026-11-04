//go:build linux

package session

import (
	"os"
	"strconv"
	"strings"
)

// processLineage returns pid and every ancestor up to init.
func processLineage(pid int) map[int]bool {
	lineage := map[int]bool{}
	for pid > 1 && !lineage[pid] {
		lineage[pid] = true
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			break
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			break
		}
		fields := strings.Fields(string(data)[end+1:])
		if len(fields) < 2 {
			break
		}
		if pid, err = strconv.Atoi(fields[1]); err != nil {
			break
		}
	}
	return lineage
}
