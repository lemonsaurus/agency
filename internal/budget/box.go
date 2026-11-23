// Package budget keeps the box under a resource ceiling. It measures what the
// box and every pane and job cost, learns what each kind of pane and job
// costs, and admits spawns and heavy jobs only while the projected total
// stays under the ceiling. What does not fit waits in a priority queue.
package budget

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// procRoot is /proc, swapped out in tests.
var procRoot = "/proc"

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat and /proc/stat times.
const clockTicks = 100

// Pressure is one PSI line: the share of time some or all tasks stalled.
type Pressure struct {
	SomeAvg10 float64 `json:"someAvg10"`
	SomeAvg60 float64 `json:"someAvg60"`
	FullAvg10 float64 `json:"fullAvg10"`
	FullAvg60 float64 `json:"fullAvg60"`
}

// Box is one reading of the whole machine.
type Box struct {
	Cores        int      `json:"cores"`
	BusyCores    float64  `json:"busyCores"` // cores busy since the previous reading
	Load1        float64  `json:"load1"`
	Load5        float64  `json:"load5"`
	Load15       float64  `json:"load15"`
	MemTotal     uint64   `json:"memTotal"`
	MemAvailable uint64   `json:"memAvailable"`
	CPU          Pressure `json:"cpuPressure"`
	Memory       Pressure `json:"memoryPressure"`
	IO           Pressure `json:"ioPressure"`
	DiskUsed     float64  `json:"diskUsed"` // percent of the filesystem holding $HOME
	cpuTotal     uint64
	cpuIdle      uint64
}

// MemUsed is what the box holds that it cannot hand out at once.
func (b Box) MemUsed() uint64 {
	if b.MemAvailable > b.MemTotal {
		return 0
	}
	return b.MemTotal - b.MemAvailable
}

// readBox reads the machine. prev supplies the CPU counters BusyCores is the
// difference from; a zero prev leaves BusyCores at zero.
func readBox(prev Box) Box {
	box := Box{Cores: runtime.NumCPU()}
	if data, err := os.ReadFile(filepath.Join(procRoot, "loadavg")); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 3 {
			box.Load1, _ = strconv.ParseFloat(fields[0], 64)
			box.Load5, _ = strconv.ParseFloat(fields[1], 64)
			box.Load15, _ = strconv.ParseFloat(fields[2], 64)
		}
	}
	box.MemTotal, box.MemAvailable = readMeminfo()
	box.cpuTotal, box.cpuIdle = readCPUTimes()
	if prev.cpuTotal > 0 && box.cpuTotal > prev.cpuTotal {
		total := float64(box.cpuTotal - prev.cpuTotal)
		idle := float64(box.cpuIdle - prev.cpuIdle)
		box.BusyCores = (total - idle) / total * float64(box.Cores)
	}
	box.CPU = readPressure("cpu")
	box.Memory = readPressure("memory")
	box.IO = readPressure("io")
	if home, err := os.UserHomeDir(); err == nil {
		var fs syscall.Statfs_t
		if syscall.Statfs(home, &fs) == nil && fs.Blocks > 0 {
			box.DiskUsed = 100 * float64(fs.Blocks-fs.Bfree) / float64(fs.Blocks-fs.Bfree+fs.Bavail)
		}
	}
	return box
}

func readMeminfo() (total, available uint64) {
	f, err := os.Open(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			available = kb * 1024
		}
	}
	return total, available
}

// readCPUTimes sums the aggregate cpu line of /proc/stat. Idle includes iowait.
func readCPUTimes() (total, idle uint64) {
	data, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return 0, 0
	}
	line, _, _ := bytes.Cut(data, []byte("\n"))
	fields := strings.Fields(string(line))
	if len(fields) < 6 || fields[0] != "cpu" {
		return 0, 0
	}
	for i, field := range fields[1:] {
		v, _ := strconv.ParseUint(field, 10, 64)
		// guest and guest_nice are already counted in user and nice.
		if i >= 8 {
			break
		}
		total += v
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return total, idle
}

func readPressure(resource string) Pressure {
	data, err := os.ReadFile(filepath.Join(procRoot, "pressure", resource))
	if err != nil {
		return Pressure{}
	}
	return parsePressure(string(data))
}

func parsePressure(text string) Pressure {
	var p Pressure
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		values := map[string]float64{}
		for _, field := range fields[1:] {
			if key, value, ok := strings.Cut(field, "="); ok {
				values[key], _ = strconv.ParseFloat(value, 64)
			}
		}
		switch fields[0] {
		case "some":
			p.SomeAvg10, p.SomeAvg60 = values["avg10"], values["avg60"]
		case "full":
			p.FullAvg10, p.FullAvg60 = values["avg10"], values["avg60"]
		}
	}
	return p
}
