//go:build darwin

package session

import (
	"os/exec"
	"strconv"
	"strings"
)

func processDescendsFrom(pid, ancestor int) bool {
	for pid > 1 {
		if pid == ancestor {
			return true
		}
		out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false
		}
		parent, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil || parent == pid {
			return false
		}
		pid = parent
	}
	return false
}
