package restore

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lemonsaurus/agency/internal/ipc"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// sessionStartSlack is how far from a Pi's start its session file may be
// created: Pi opens the file while it boots.
const sessionStartSlack = 20 * time.Second

// SessionFile finds the Pi session file of a pane: the one its Pi bridge
// announced, else the one its command resumed, else the file Pi created in the
// pane's directory when the process started. A found file is remembered for
// the life of the pane.
func SessionFile(socket string) func(pane tmux.PaneInfo, command string) string {
	var mu sync.Mutex
	found := map[string]string{}
	return func(pane tmux.PaneInfo, command string) string {
		if path, err := ipc.SessionPath(socket, pane.ID); err == nil {
			if data, err := os.ReadFile(path); err == nil {
				if file := strings.TrimSpace(string(data)); file != "" {
					return file
				}
			}
		}
		if file := commandSession(command); file != "" {
			return file
		}
		if !isPi(command) {
			return ""
		}
		key := pane.ID + ":" + strconv.Itoa(pane.PID)
		mu.Lock()
		defer mu.Unlock()
		if file, ok := found[key]; ok {
			return file
		}
		file := createdAtStart(pane)
		if file != "" {
			found[key] = file
		}
		return file
	}
}

// createdAtStart is the session file in the pane's directory whose name
// carries the time closest to when the pane's process started.
func createdAtStart(pane tmux.PaneInfo) string {
	started, ok := processStart(pane.PID)
	if !ok || pane.CWD == "" {
		return ""
	}
	dir := piSessionDir(pane.CWD)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, distance := "", sessionStartSlack
	for _, entry := range entries {
		created, ok := nameTime(entry.Name())
		if !ok {
			continue
		}
		if gap := created.Sub(started).Abs(); gap <= distance {
			best, distance = filepath.Join(dir, entry.Name()), gap
		}
	}
	return best
}

// piSessionDir is where Pi keeps the conversations started in cwd.
func piSessionDir(cwd string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent", "sessions", "--"+strings.ReplaceAll(strings.Trim(cwd, "/"), "/", "-")+"--")
}

// nameTime reads the UTC time at the front of a Pi session file name, such as
// 2026-11-27T09-43-42-669Z_<id>.jsonl.
func nameTime(name string) (time.Time, bool) {
	stamp, _, ok := strings.Cut(name, "_")
	if !ok || !strings.HasSuffix(name, ".jsonl") || len(stamp) != len("2026-11-27T09-43-42-669Z") {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02T15-04-05", stamp[:19])
	if err != nil {
		return time.Time{}, false
	}
	millis, err := strconv.Atoi(stamp[20:23])
	return t.Add(time.Duration(millis) * time.Millisecond), err == nil
}

// processStart is when a process started, from /proc.
func processStart(pid int) (time.Time, bool) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, false
	}
	// The command name is parenthesised and may hold spaces; the fields after it start at the state.
	text := string(stat)
	fields := strings.Fields(text[strings.LastIndex(text, ")")+1:])
	if len(fields) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	system, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(system), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(boot, 0).Add(time.Duration(ticks) * time.Second / 100), true
		}
	}
	return time.Time{}, false
}

// commandSession is the file after --session in a Pi command line.
func commandSession(command string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		if field == "--session" && i+1 < len(fields) {
			return strings.Trim(fields[i+1], "'")
		}
		if value, ok := strings.CutPrefix(field, "--session="); ok {
			return value
		}
	}
	return ""
}

// ResumeCommand is the command that continues a Pi conversation: command
// with any session flag swapped for the saved file.
func ResumeCommand(command, session string) string {
	fields := strings.Fields(command)
	var kept []string
	for i := 0; i < len(fields); i++ {
		switch {
		case fields[i] == "--session":
			i++
		case strings.HasPrefix(fields[i], "--session="):
		default:
			kept = append(kept, fields[i])
		}
	}
	return strings.Join(append(kept, "--session", shellQuote(session)), " ")
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-:+@=,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
