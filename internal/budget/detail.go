package budget

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Process is one process in a pane's or job's tree, for `agency budget --json`.
type Process struct {
	PID        int       `json:"pid"`
	PPID       int       `json:"ppid"`
	Argv       []string  `json:"argv"`
	CWD        string    `json:"cwd,omitempty"`
	Started    time.Time `json:"started"`
	CPUSeconds float64   `json:"cpuSeconds"`
	CPUPercent float64   `json:"cpuPercent"`
	RSS        uint64    `json:"rss"`
	Ports      []int     `json:"ports,omitempty"` // TCP ports it listens on
}

// Inspect describes pids, measuring CPU over window.
func Inspect(pids []int, window time.Duration) []Process {
	before := readProcs()
	time.Sleep(window)
	after := readProcs()
	listeners := listeningInodes()
	var out []Process
	for _, pid := range pids {
		p, ok := after.procs[pid]
		if !ok {
			continue
		}
		own := p.ticks
		process := Process{
			PID:        pid,
			PPID:       p.ppid,
			Argv:       cmdline(pid),
			RSS:        p.rss,
			CPUSeconds: float64(own) / clockTicks,
			Started:    after.started(pid, time.Time{}),
		}
		if b, ok := before.procs[pid]; ok && b.start == p.start && p.ticks >= b.ticks {
			process.CPUPercent = 100 * float64(p.ticks-b.ticks) / clockTicks / window.Seconds()
		}
		process.CWD, _ = os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "cwd"))
		process.Ports = ports(pid, listeners)
		out = append(out, process)
	}
	return out
}

// Strays are processes in agency's own units that belong to no live pane or
// job tree: the daemon's and tmux server's leftovers, and a dead pane's
// survivors. keep holds the pids that do belong somewhere.
func Strays(units func(pid int) string, isAgencyUnit func(unit string) bool, keep map[int]bool) []int {
	table := readProcs()
	var pids []int
	for pid := range table.procs {
		if !keep[pid] && isAgencyUnit(units(pid)) {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids
}

func bootTime() time.Time {
	f, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if rest, ok := strings.CutPrefix(scanner.Text(), "btime "); ok {
			secs, _ := strconv.ParseInt(rest, 10, 64)
			return time.Unix(secs, 0)
		}
	}
	return time.Time{}
}

// listeningInodes maps each listening TCP socket's inode to its port.
func listeningInodes() map[string]int {
	inodes := map[string]int{}
	for _, name := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(procRoot, "net", name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			f := strings.Fields(line)
			// local_address is f[1], st is f[3] (0A is LISTEN), inode is f[9].
			if len(f) < 10 || f[3] != "0A" {
				continue
			}
			_, portHex, ok := strings.Cut(f[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseInt(portHex, 16, 32)
			if err == nil {
				inodes[f[9]] = int(port)
			}
		}
	}
	return inodes
}

func ports(pid int, listeners map[string]int) []int {
	if len(listeners) == 0 {
		return nil
	}
	dir := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:[") {
			continue
		}
		if port, ok := listeners[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")]; ok && !seen[port] {
			seen[port] = true
			out = append(out, port)
		}
	}
	sort.Ints(out)
	return out
}
