// Package tmuxtest cleans up the tmux servers tests start. A server started
// with the cloud conf refuses kill-server, so tests end it by PID, and a
// test run that died before its cleanup leaves servers a later run sweeps.
package tmuxtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Kill ends the tmux server on the named socket and removes the socket.
func Kill(socket string) {
	out, err := exec.Command("tmux", "-L", socket, "display-message", "-p", "#{pid}").Output()
	if err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && pid > 1 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	os.Remove(filepath.Join(os.TempDir(), fmt.Sprintf("tmux-%d", os.Getuid()), socket))
}

// owner matches a test server's socket name: the PID of the test process that
// started it follows "test-".
var owner = regexp.MustCompile(`-test-(\d+)(-|$)`)

// Sweep ends the servers of test processes that no longer exist.
func Sweep() {
	sockets, _ := filepath.Glob(filepath.Join(os.TempDir(), fmt.Sprintf("tmux-%d", os.Getuid()), "agency-*"))
	for _, path := range sockets {
		name := filepath.Base(path)
		match := owner.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		pid, _ := strconv.Atoi(match[1])
		if err := syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
			continue
		}
		Kill(name)
	}
}
