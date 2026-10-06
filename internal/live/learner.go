package live

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Learner is what Rosa knows about Lemon as a student, kept across calls.
type Learner struct {
	Profile  []string                 `json:"profile"`
	Thoughts map[string]ThoughtStatus `json:"thoughts"`
	Used     []string                 `json:"used"`
	Errors   []LearnerError           `json:"errors"`
	Habits   []string                 `json:"habits"`
	Pacing   []Pace                   `json:"pacing"`
	Calls    []LessonCall             `json:"calls"`
}

// ThoughtStatus is introduced, found with help, or found alone; a thought never introduced is absent.
type ThoughtStatus struct {
	Status string    `json:"status"`
	Lesson string    `json:"lesson"`
	At     time.Time `json:"at"`
}

// LearnerError is a wrong answer filed under its diagnosed cause.
type LearnerError struct {
	At       time.Time `json:"at"`
	Thought  string    `json:"thought,omitempty"`
	Said     string    `json:"said,omitempty"`
	Expected string    `json:"expected,omitempty"`
	Cause    string    `json:"cause"`
}

// Pace is one answered prompt: how long Lemon thought before answering, and how it went.
type Pace struct {
	At       time.Time `json:"at"`
	Sentence string    `json:"sentence"`
	ThinkMS  int64     `json:"think_ms"`
	Outcome  string    `json:"outcome"`
}

// LessonCall is one call: when, how long, which thoughts it covered, and the debrief.
type LessonCall struct {
	At       time.Time `json:"at"`
	Session  string    `json:"session"`
	Minutes  float64   `json:"minutes"`
	Thoughts []string  `json:"thoughts"`
	Summary  string    `json:"summary,omitempty"`
}

const (
	learnerUsed   = 400
	learnerErrors = 200
	learnerPacing = 600
	learnerCalls  = 200
)

// LearnerStore keeps Learner in one JSON file, rewritten whole on every change.
type LearnerStore struct {
	mu   sync.Mutex
	path string
	data Learner
}

func OpenLearner(path string) *LearnerStore {
	s := &LearnerStore{path: path, data: Learner{Thoughts: map[string]ThoughtStatus{}}}
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, &s.data)
	}
	if s.data.Thoughts == nil {
		s.data.Thoughts = map[string]ThoughtStatus{}
	}
	return s
}

// Update changes the learner under the lock and saves it.
func (s *LearnerStore) Update(change func(l *Learner)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.data)
	s.data.Used = tail(s.data.Used, learnerUsed)
	s.data.Errors = tail(s.data.Errors, learnerErrors)
	s.data.Pacing = tail(s.data.Pacing, learnerPacing)
	s.data.Calls = tail(s.data.Calls, learnerCalls)
	if s.path == "" {
		return nil
	}
	data, _ := json.MarshalIndent(s.data, "", "  ")
	os.MkdirAll(filepath.Dir(s.path), 0o700)
	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, s.path)
}

// Status is a thought's status, "not introduced" when it never came up.
func (s *LearnerStore) Status(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if status, ok := s.data.Thoughts[id]; ok {
		return status.Status
	}
	return "not introduced"
}

// Summary is the learner as notes for Rosa's models: profile, thought statuses, recent errors and
// habits, sentences already used, and how the last calls went.
func (s *LearnerStore) Summary(now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	b.WriteString("Learner memory.")
	if len(s.data.Profile) > 0 {
		b.WriteString(" About Lemon: " + strings.Join(s.data.Profile, " "))
	}
	if len(s.data.Thoughts) > 0 {
		b.WriteString(" Thoughts so far:")
		for id, status := range s.data.Thoughts {
			fmt.Fprintf(&b, " %s %s;", id, status.Status)
		}
	}
	if len(s.data.Errors) > 0 {
		b.WriteString(" Recent errors by cause:")
		for _, e := range tail(s.data.Errors, 8) {
			fmt.Fprintf(&b, " [%s] said %q for %q: %s;", e.Thought, e.Said, e.Expected, e.Cause)
		}
	}
	if len(s.data.Habits) > 0 {
		b.WriteString(" Habits: " + strings.Join(s.data.Habits, " "))
	}
	if len(s.data.Used) > 0 {
		b.WriteString(" Sentences already used, vary them: " + strings.Join(tail(s.data.Used, 30), "; ") + ".")
	}
	if len(s.data.Calls) == 0 {
		b.WriteString(" This is Lemon's first lesson with you.")
	}
	for _, call := range tail(s.data.Calls, 3) {
		fmt.Fprintf(&b, " Call %s, %.0f minutes, thoughts %s: %s", ago(now.Sub(call.At)), call.Minutes, strings.Join(call.Thoughts, ", "), call.Summary)
	}
	return b.String()
}

// JSON is the whole learner record.
func (s *LearnerStore) JSON() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.data)
	return string(data)
}

func tail[T any](items []T, keep int) []T {
	if len(items) > keep {
		return items[len(items)-keep:]
	}
	return items
}
