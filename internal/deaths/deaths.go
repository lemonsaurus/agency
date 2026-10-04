// Package deaths keeps the pane death log: one JSON line for every pane that
// exits, dies, is killed, or vanishes, with enough context to say why.
package deaths

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Event kinds. killed, died, exited and gone end a pane's life; handoff,
// daemon and guard explain what happened around it.
const (
	KindKilled  = "killed"  // agency killed the pane on someone's request
	KindDied    = "died"    // the pane's process exited non-zero or by signal (tmux pane-died)
	KindExited  = "exited"  // the pane's process exited with status 0 (tmux pane-exited)
	KindGone    = "gone"    // the daemon noticed a tracked pane disappear
	KindHandoff = "handoff" // a successor replaced the pane
	KindDaemon  = "daemon"  // the agency daemon started
	KindGuard   = "guard"   // agency installed the systemd OOM guard
)

// MaxBytes is where the log rotates into a single .1 generation.
const MaxBytes = 4 << 20

// Actor is whoever asked agency to kill a pane.
type Actor struct {
	Pane    string `json:"pane,omitempty"`
	Role    string `json:"role,omitempty"`
	Human   bool   `json:"human,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Command string `json:"command,omitempty"`
}

// OOM is the systemd evidence for an out-of-memory kill.
type OOM struct {
	Unit string `json:"unit"`
	Peak string `json:"peak,omitempty"`
	// Exact is true when systemd named this pane's pid among the processes it
	// killed while stopping the OOM-killed unit.
	Exact bool `json:"exact"`
}

// Event is one line of the death log.
type Event struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	Pane      string    `json:"pane,omitempty"`
	PID       int       `json:"pid,omitempty"`
	Label     string    `json:"label,omitempty"`
	Role      string    `json:"role,omitempty"`
	Group     string    `json:"group,omitempty"`
	Dir       string    `json:"dir,omitempty"`
	Command   string    `json:"command,omitempty"`
	Status    *int      `json:"status,omitempty"`
	Signal    string    `json:"signal,omitempty"`
	OOM       *OOM      `json:"oom,omitempty"`
	By        *Actor    `json:"by,omitempty"`
	Successor string    `json:"successor,omitempty"`
	Memory    string    `json:"memory,omitempty"`
	Note      string    `json:"note,omitempty"`
}

// Log appends events to a JSONL file. A nil *Log records nothing.
type Log struct {
	Path string
}

// DefaultPath is ~/.agents/run/agency/pane-deaths.jsonl, beside the activity diary.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agency-pane-deaths.jsonl")
	}
	return filepath.Join(home, ".agents", "run", "agency", "pane-deaths.jsonl")
}

// Open returns the log at DefaultPath.
func Open() *Log {
	return &Log{Path: DefaultPath()}
}

// Record appends e, stamping the time and free memory when unset. Failures go
// to the daemon log; a death must never fail the operation that caused it.
func (l *Log) Record(e Event) {
	if l == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if e.Memory == "" && e.Kind != KindHandoff && e.Kind != KindGuard {
		e.Memory = Memory()
	}
	if err := l.append(e); err != nil {
		log.Printf("deaths: recording %s %s: %v", e.Kind, e.Pane, err)
	}
}

func (l *Log) append(e Event) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return err
	}
	// The daemon and every tmux hook write here; the lock spans rotation and append.
	lock, err := os.OpenFile(l.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if info, err := os.Stat(l.Path); err == nil && info.Size()+int64(len(line)) >= MaxBytes {
		if err := os.Rename(l.Path, l.Path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// Read returns events at or after since, oldest first, from the rotated
// generation and the live file. Malformed lines are skipped.
func (l *Log) Read(since time.Time) ([]Event, error) {
	var events []Event
	for _, path := range []string{l.Path + ".1", l.Path} {
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			var e Event
			if json.Unmarshal(scanner.Bytes(), &e) != nil || e.At.Before(since) {
				continue
			}
			events = append(events, e)
		}
		err = scanner.Err()
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	return events, nil
}

// Caller is a live process's command line, for attributing kill requests.
func Caller(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return ""
	}
	args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	args[0] = filepath.Base(args[0])
	command := strings.Join(args, " ")
	if len(command) > 300 {
		command = command[:300] + "…"
	}
	return command
}

// Memory is "available of total" from /proc/meminfo, e.g. "1.2G of 46.1G free".
func Memory() string {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	return parseMeminfo(string(data))
}

func parseMeminfo(data string) string {
	fields := map[string]float64{}
	for _, line := range strings.Split(data, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		kb, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 64)
		if err == nil {
			fields[name] = kb
		}
	}
	total, available := fields["MemTotal"], fields["MemAvailable"]
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%.1fG of %.1fG free", available/(1<<20), total/(1<<20))
}

var signalNames = map[int]string{
	1: "SIGHUP", 2: "SIGINT", 3: "SIGQUIT", 4: "SIGILL", 5: "SIGTRAP", 6: "SIGABRT",
	7: "SIGBUS", 8: "SIGFPE", 9: "SIGKILL", 10: "SIGUSR1", 11: "SIGSEGV", 12: "SIGUSR2",
	13: "SIGPIPE", 14: "SIGALRM", 15: "SIGTERM", 24: "SIGXCPU", 25: "SIGXFSZ",
}

// SignalName turns tmux's #{pane_dead_signal} number into SIGKILL and friends.
func SignalName(number string) string {
	n, err := strconv.Atoi(strings.TrimSpace(number))
	if err != nil || n <= 0 {
		return ""
	}
	if name, ok := signalNames[n]; ok {
		return name
	}
	return "signal " + strconv.Itoa(n)
}
