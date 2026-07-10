//go:build linux

package session

import (
	"os"
	"strconv"
	"strings"
)

func processDescendsFrom(pid, ancestor int) bool {
	for pid > 1 {
		if pid == ancestor {
			return true
		}
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return false
		}
		fields := strings.Fields(string(data)[end+1:])
		if len(fields) < 2 {
			return false
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil || parent == pid {
			return false
		}
		pid = parent
	}
	return false
}
