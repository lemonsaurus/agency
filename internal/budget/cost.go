package budget

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// GB is a gibibyte, the unit costs are written in.
const GB = 1 << 30

// Cost is CPU in cores and memory in bytes.
type Cost struct {
	CPU float64 `json:"cpu"`
	Mem uint64  `json:"mem"`
}

func (c Cost) add(o Cost) Cost { return Cost{c.CPU + o.CPU, c.Mem + o.Mem} }

// over is what c needs beyond o, never negative.
func (c Cost) over(o Cost) Cost {
	extra := Cost{CPU: math.Max(0, c.CPU-o.CPU)}
	if c.Mem > o.Mem {
		extra.Mem = c.Mem - o.Mem
	}
	return extra
}

func (c Cost) fits(ceiling Cost) bool { return c.CPU <= ceiling.CPU && c.Mem <= ceiling.Mem }

func (c Cost) String() string {
	return fmt.Sprintf("%.1f cores, %s", c.CPU, FormatBytes(c.Mem))
}

// FormatBytes renders bytes as GB with one decimal, or MB below a gigabyte.
func FormatBytes(b uint64) string {
	if b < GB {
		return fmt.Sprintf("%d MB", b>>20)
	}
	return fmt.Sprintf("%.1f GB", float64(b)/GB)
}

// ParseCost reads "4c,6g", "4c", or "6g". Missing parts stay zero.
func ParseCost(s string) (Cost, error) {
	var c Cost
	for _, part := range splitComma(s) {
		var v float64
		var unit string
		if _, err := fmt.Sscanf(part, "%g%s", &v, &unit); err != nil || v < 0 {
			return c, fmt.Errorf("cost %q: want cores and memory like 4c,6g", s)
		}
		switch unit {
		case "c":
			c.CPU = v
		case "g":
			c.Mem = uint64(v * GB)
		case "m":
			c.Mem = uint64(v * (1 << 20))
		default:
			return c, fmt.Errorf("cost %q: unit %q is not c, g, or m", s, unit)
		}
	}
	return c, nil
}

func splitComma(s string) []string {
	var parts []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				parts = append(parts, s[start:i])
			}
			start = i + 1
		}
	}
	return parts
}

// learned is what one class of pane or job has cost so far.
type learned struct {
	CPU     float64   `json:"cpu"`
	Mem     float64   `json:"mem"`
	Samples int       `json:"samples"`
	Updated time.Time `json:"updated"`
}

// Learner keeps a moving estimate of each class's cost and saves it, so a
// daemon restart keeps what it learned.
type Learner struct {
	mu      sync.Mutex
	path    string
	classes map[string]*learned
	dirty   bool
	saved   time.Time
}

// paneAlpha weighs one five-second sample of a running pane; jobAlpha weighs
// one finished job.
const (
	paneAlpha = 0.02
	jobAlpha  = 0.3
)

// margin pads learned costs, since a class's next instance may run hotter.
const margin = 1.25

func newLearner(path string) *Learner {
	l := &Learner{path: path, classes: map[string]*learned{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &l.classes)
	}
	return l
}

func (l *Learner) observe(class string, used Cost, alpha float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.classes[class]
	if c == nil {
		c = &learned{CPU: used.CPU, Mem: float64(used.Mem)}
		l.classes[class] = c
	} else {
		// The first few samples count fully, so a class settles fast.
		a := math.Max(alpha, 1/float64(c.Samples+1))
		c.CPU += a * (used.CPU - c.CPU)
		c.Mem += a * (float64(used.Mem) - c.Mem)
	}
	c.Samples++
	c.Updated = time.Now()
	l.dirty = true
}

// estimate is the padded learned cost of class, or fallback before any
// instance of it was seen.
func (l *Learner) estimate(class string, fallback Cost) Cost {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.classes[class]
	if c == nil || c.Samples < 3 {
		return fallback
	}
	return Cost{CPU: c.CPU * margin, Mem: uint64(c.Mem * margin)}
}

// save writes the estimates at most once a minute.
func (l *Learner) save(force bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.dirty || l.path == "" || !force && time.Since(l.saved) < time.Minute {
		return
	}
	data, err := json.MarshalIndent(l.classes, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(l.path), 0o755)
	tmp := l.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil && os.Rename(tmp, l.path) == nil {
		l.dirty, l.saved = false, time.Now()
	}
}
