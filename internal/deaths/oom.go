package deaths

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// oomWindow is how far before a death an OOM kill in another unit still counts as the probable cause.
const oomWindow = 60 * time.Second

var (
	killingProcess = regexp.MustCompile(`^Killing process (\d+) \(`)
	memoryPeak     = regexp.MustCompile(`([0-9.]+[KMGT]?) memory peak`)
)

// ProbeOOM asks the user journal whether systemd's OOM handling killed pid
// around at. systemd may still be writing the unit's teardown when tmux
// reports the death, so it looks a few times before giving up.
func ProbeOOM(ctx context.Context, pid int, at time.Time) *OOM {
	for attempt := 0; ; attempt++ {
		cmd := exec.CommandContext(ctx, "journalctl", "--user", "--no-pager", "--output=json",
			"--output-fields=MESSAGE,USER_UNIT,UNIT",
			"--since=@"+strconv.FormatInt(at.Add(-oomWindow).Unix(), 10))
		out, err := cmd.Output()
		if err != nil {
			return nil
		}
		oom := parseJournal(strings.NewReader(string(out)), pid, at)
		if oom != nil && oom.Peak != "" || attempt == 2 {
			return oom
		}
		select {
		case <-ctx.Done():
			return oom
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

// parseJournal reads `journalctl --output=json` lines from the user manager.
// It matches systemd's "Killing process <pid>" teardown line for an exact
// hit, and otherwise takes the latest OOM kill within oomWindow before at.
func parseJournal(r io.Reader, pid int, at time.Time) *OOM {
	type unitState struct {
		oomAt time.Time
		peak  string
	}
	units := map[string]*unitState{}
	exactUnit := ""
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		var entry struct {
			Message  string `json:"MESSAGE"`
			UserUnit string `json:"USER_UNIT"`
			Unit     string `json:"UNIT"`
			Realtime string `json:"__REALTIME_TIMESTAMP"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		unit := entry.UserUnit
		if unit == "" {
			unit = entry.Unit
		}
		if unit == "" {
			continue
		}
		state := units[unit]
		if state == nil {
			state = &unitState{}
			units[unit] = state
		}
		micros, _ := strconv.ParseInt(entry.Realtime, 10, 64)
		when := time.UnixMicro(micros)
		switch {
		case strings.Contains(entry.Message, "killed by the OOM killer"):
			state.oomAt = when
		case memoryPeak.MatchString(entry.Message):
			state.peak = memoryPeak.FindStringSubmatch(entry.Message)[1]
		default:
			if match := killingProcess.FindStringSubmatch(entry.Message); match != nil && match[1] == strconv.Itoa(pid) {
				exactUnit = unit
			}
		}
	}
	if state := units[exactUnit]; exactUnit != "" && !state.oomAt.IsZero() {
		return &OOM{Unit: exactUnit, Peak: state.peak, Exact: true}
	}
	var best *OOM
	var bestAt time.Time
	for unit, state := range units {
		if state.oomAt.IsZero() || state.oomAt.Before(at.Add(-oomWindow)) || state.oomAt.After(at.Add(5*time.Second)) {
			continue
		}
		if best == nil || state.oomAt.After(bestAt) {
			best, bestAt = &OOM{Unit: unit, Peak: state.peak}, state.oomAt
		}
	}
	return best
}
