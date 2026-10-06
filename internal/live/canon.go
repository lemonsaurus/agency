package live

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// CanonEntry is one thing true about Rosa: a fact, a person, a place, a story, a teaser, or an open
// serial with its hook for next time. Told stamps every call she told it in.
type CanonEntry struct {
	ID      string      `json:"id"`
	Kind    string      `json:"kind"`
	Title   string      `json:"title"`
	Text    string      `json:"text"`
	Spanish []string    `json:"spanish,omitempty"`
	Next    string      `json:"next,omitempty"`
	Rating  string      `json:"rating,omitempty"`
	Source  string      `json:"source,omitempty"`
	Told    []time.Time `json:"told,omitempty"`
}

// Canon is Rosa's life, kept in the run directory, one entry per line. It starts as a copy of the
// seed in dotagents and grows as the planner authors new entries.
type Canon struct {
	mu      sync.Mutex
	path    string
	entries []CanonEntry
}

// OpenCanon reads the canon at path, seeding it from seed the first time.
func OpenCanon(path, seed string) *Canon {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if data, err := os.ReadFile(seed); err == nil {
			os.WriteFile(path, data, 0o600)
		}
	}
	c := &Canon{path: path}
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(data), "\n") {
		var entry CanonEntry
		if strings.TrimSpace(line) != "" && json.Unmarshal([]byte(line), &entry) == nil && entry.ID != "" {
			c.entries = append(c.entries, entry)
		}
	}
	return c
}

func (c *Canon) save() error {
	var b strings.Builder
	for _, entry := range c.entries {
		data, _ := json.Marshal(entry)
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(c.path+".tmp", []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(c.path+".tmp", c.path)
}

// All is every entry.
func (c *Canon) All() []CanonEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]CanonEntry(nil), c.entries...)
}

// Add authors a new entry within her persona.
func (c *Canon) Add(entry CanonEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry.ID == "" || entry.Text == "" {
		return fmt.Errorf("a canon entry needs an id and text")
	}
	for _, existing := range c.entries {
		if existing.ID == entry.ID {
			return fmt.Errorf("canon entry %s already exists", entry.ID)
		}
	}
	entry.Source, entry.Told = "rosa", nil
	c.entries = append(c.entries, entry)
	return c.save()
}

// Tell stamps an entry as told now, and updates an open story's hook for next time.
func (c *Canon) Tell(id, next string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.entries {
		if c.entries[i].ID == id {
			c.entries[i].Told = append(c.entries[i].Told, now)
			if next != "" {
				c.entries[i].Next = next
			}
			return c.save()
		}
	}
	return fmt.Errorf("no canon entry %s", id)
}

// Summary lists the canon for the planner: untold entries first, then told ones with how often and the
// hooks of open stories.
func (c *Canon) Summary(now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var untold, told []string
	for _, entry := range c.entries {
		line := fmt.Sprintf("%s (%s, %s): %s", entry.ID, entry.Kind, entry.Rating, entry.Title)
		if entry.Next != "" {
			line += "; next time: " + entry.Next
		}
		if len(entry.Told) == 0 {
			untold = append(untold, line)
		} else {
			told = append(told, fmt.Sprintf("%s, told %d times, last %s", line, len(entry.Told), ago(now.Sub(entry.Told[len(entry.Told)-1]))))
		}
	}
	return "Rosa's canon, never contradict it. Not told yet: " + strings.Join(untold, "; ") + ". Told before, don't retell unless it's a serial: " + strings.Join(told, "; ") + "."
}
