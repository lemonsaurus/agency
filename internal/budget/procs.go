package budget

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// pageSize converts /proc/<pid>/stat rss pages to bytes.
var pageSize = uint64(os.Getpagesize())

// proc is one process as /proc/<pid>/stat shows it.
type proc struct {
	pid, ppid int
	start     uint64 // clock ticks after boot, with pid it names one process
	ticks     uint64 // utime+stime plus the reaped children's cutime+cstime
	rss       uint64 // bytes
}

// procTable is every process on the box at one moment.
type procTable struct {
	at       time.Time
	boot     time.Time
	procs    map[int]proc
	children map[int][]int
}

func readProcs() procTable {
	table := procTable{at: time.Now(), boot: bootTime(), procs: map[int]proc{}, children: map[int][]int{}}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return table
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "stat"))
		if err != nil {
			continue
		}
		p, ok := parseStat(pid, data)
		if !ok {
			continue
		}
		table.procs[pid] = p
		table.children[p.ppid] = append(table.children[p.ppid], pid)
	}
	return table
}

// parseStat reads the fields after the command, which may itself hold spaces
// and parentheses.
func parseStat(pid int, data []byte) (proc, bool) {
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return proc{}, false
	}
	fields := strings.Fields(string(data[end+1:]))
	// fields[0] is state (field 3); ppid is field 4, utime 14 ... starttime 22, rss 24.
	if len(fields) < 22 {
		return proc{}, false
	}
	n := func(field int) uint64 {
		v, _ := strconv.ParseUint(fields[field-3], 10, 64)
		return v
	}
	return proc{
		pid:   pid,
		ppid:  int(n(4)),
		start: n(22),
		ticks: n(14) + n(15) + n(16) + n(17),
		rss:   n(24) * pageSize,
	}, true
}

// started is when pid started, or fallback when it is gone.
func (t procTable) started(pid int, fallback time.Time) time.Time {
	p, ok := t.procs[pid]
	if !ok || t.boot.IsZero() {
		return fallback
	}
	return t.boot.Add(time.Duration(p.start) * time.Second / clockTicks)
}

// tree returns root and every descendant still running, stopping at pids in
// skip so a job's processes count only toward the job.
func (t procTable) tree(root int, skip map[int]bool) []int {
	if _, ok := t.procs[root]; !ok {
		return nil
	}
	pids := []int{root}
	for i := 0; i < len(pids); i++ {
		for _, child := range t.children[pids[i]] {
			if !skip[child] {
				pids = append(pids, child)
			}
		}
	}
	return pids
}

// usage sums what pids hold now.
func (t procTable) usage(pids []int) (ticks, rss uint64) {
	for _, pid := range pids {
		p := t.procs[pid]
		ticks += p.ticks
		rss += p.rss
	}
	return ticks, rss
}

// cmdline is the argv of pid, empty when it is gone or a kernel thread.
func cmdline(pid int) []string {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
}

// Trees maps each root to it and its running descendants, from one read of /proc.
func Trees(roots []int) map[int][]int {
	table := readProcs()
	trees := make(map[int][]int, len(roots))
	for _, root := range roots {
		trees[root] = table.tree(root, nil)
	}
	return trees
}
