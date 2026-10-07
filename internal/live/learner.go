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
	Gaps     []GapRecord              `json:"gaps"`
	Words    map[string]Word          `json:"words"`
	Heard    map[string]Word          `json:"heard"`
	Facts    []Fact                   `json:"facts"`
	Topics   []TopicUse               `json:"topics"`
	Cautions []string                 `json:"cautions"`
	Links    []string                 `json:"links"`
	Calls    []LessonCall             `json:"calls"`
}

// Fact is something Lemon told Rosa about his life, filed under a domain. An unconfirmed fact is
// asked about, never stated.
type Fact struct {
	Domain    string    `json:"domain"`
	Text      string    `json:"text"`
	Confirmed bool      `json:"confirmed"`
	At        time.Time `json:"at"`
}

// TopicUse is a topic a call's sentences or stories were about.
type TopicUse struct {
	Topic string    `json:"topic"`
	At    time.Time `json:"at"`
}

// Domains are the parts of his life Rosa asks about, fewest known facts first.
var Domains = []string{"family", "Sofie", "work", "music", "home", "food", "plans", "today", "travel", "childhood", "friends", "hobbies"}

// hear records the words of a Spanish chunk he understood by ear.
func (l *Learner) hear(chunk string, now time.Time) {
	if l.Heard == nil {
		l.Heard = map[string]Word{}
	}
	for _, word := range words(chunk) {
		entry, ok := l.Heard[word]
		if !ok {
			entry = Word{Word: word, First: now, How: "understood"}
		}
		entry.Last, entry.Sentence = now, truncate(chunk, 200)
		entry.Seen++
		l.Heard[word] = entry
	}
}

// Cautions are the latest mistakes Rosa made, for the next call's first note.
func (s *LearnerStore) Cautions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return tail(append([]string(nil), s.data.Cautions...), 6)
}

// Word is one Spanish word Lemon has been exposed to, keyed by its accentless lowercase form. How is
// the best way he has got it: told, found with help, or found alone. Kind is word, or what he has
// memorised: gender (with Gender, el or la), endings (a set of conjugation endings), or form (an
// irregular form).
type Word struct {
	Word     string    `json:"word"`
	Kind     string    `json:"kind,omitempty"`
	Gender   string    `json:"gender,omitempty"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	Seen     int       `json:"seen"`
	How      string    `json:"how"`
	Sentence string    `json:"sentence"`
	Links    []string  `json:"links,omitempty"`
}

var hows = map[string]int{"told": 1, "found with help": 2, "found alone": 3}

// expose records the words of a Spanish sentence as seen, keeping the best way he has got each.
func (l *Learner) expose(sentence, how string, now time.Time) {
	if l.Words == nil {
		l.Words = map[string]Word{}
	}
	for _, raw := range strings.Fields(sentence) {
		word := strings.ToLower(strings.Trim(raw, ".,;:!?¡¿\"'()…"))
		key := strings.Join(words(word), "")
		if key == "" {
			continue
		}
		entry, ok := l.Words[key]
		if !ok {
			entry = Word{Word: word, First: now}
		}
		entry.Last, entry.Sentence = now, truncate(sentence, 200)
		entry.Seen++
		if hows[how] > hows[entry.How] {
			entry.How = how
		}
		l.Words[key] = entry
	}
}

// Known is whether every word of a Spanish chunk is in the dictionary, singular or plural.
func (s *LearnerStore) Known(chunk string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens := words(chunk)
	if len(tokens) == 0 {
		return false
	}
	for _, token := range tokens {
		found := false
		for _, form := range []string{token, strings.TrimSuffix(token, "s"), strings.TrimSuffix(token, "es"), token + "s"} {
			if _, ok := s.data.Words[form]; ok {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// GapRecord is one English chunk from a code-switched turn: found by its route, or not.
type GapRecord struct {
	At      time.Time `json:"at"`
	English string    `json:"english"`
	Spanish string    `json:"spanish"`
	Route   string    `json:"route"`
	Thought string    `json:"thought,omitempty"`
	Found   bool      `json:"found"`
}

// ThoughtStatus is introduced, found with help, or found alone; a thought never introduced is absent.
// Seen counts the calls that touched it and Alone the calls where he found it alone.
type ThoughtStatus struct {
	Status string    `json:"status"`
	Seen   int       `json:"seen"`
	Alone  int       `json:"alone"`
	At     time.Time `json:"at"`
}

// Due is whether a thought should come back for hidden repetition: always until he finds it alone,
// then after a gap that doubles with every call he finds it alone.
func (t ThoughtStatus) Due(now time.Time) bool {
	if t.Status != "found alone" {
		return true
	}
	return now.Sub(t.At) >= 24*time.Hour<<min(max(t.Alone-1, 0), 8)
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
	learnerGaps   = 400
	learnerLinks  = 100
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
	s.data.Gaps = tail(s.data.Gaps, learnerGaps)
	s.data.Links = tail(s.data.Links, learnerLinks)
	s.data.Facts = tail(s.data.Facts, 300)
	s.data.Topics = tail(s.data.Topics, 300)
	s.data.Cautions = tail(s.data.Cautions, 40)
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
	for _, fact := range s.data.Facts {
		if fact.Confirmed {
			fmt.Fprintf(&b, " [%s] %s.", fact.Domain, fact.Text)
		} else {
			fmt.Fprintf(&b, " [%s, unconfirmed: ask him, never state it] %s.", fact.Domain, fact.Text)
		}
	}
	if len(s.data.Thoughts) > 0 {
		counts := map[string]int{}
		for _, status := range s.data.Thoughts {
			counts[status.Status]++
		}
		fmt.Fprintf(&b, " Thoughts: %d found alone, %d found with help, %d introduced.", counts["found alone"], counts["found with help"], counts["introduced"])
	}
	if len(s.data.Errors) > 0 {
		b.WriteString(" Recent errors by cause:")
		for _, e := range tail(s.data.Errors, 8) {
			fmt.Fprintf(&b, " [%s] said %q for %q: %s;", e.Thought, e.Said, e.Expected, e.Cause)
		}
	}
	if len(s.data.Words) > 0 {
		fmt.Fprintf(&b, " Dictionary: %d Spanish words met.", len(s.data.Words))
	}
	if len(s.data.Links) > 0 {
		b.WriteString(" Cross-language links he reacted to, make more of these kinds: " + strings.Join(tail(s.data.Links, 8), " "))
	}
	if len(s.data.Habits) > 0 {
		b.WriteString(" Habits: " + strings.Join(s.data.Habits, " "))
	}
	var recycle, reinforce []string
	for _, gap := range tail(s.data.Gaps, 40) {
		switch {
		case gap.Found:
		case gap.Route == "new":
			recycle = append(recycle, gap.English+" = "+gap.Spanish)
		default:
			reinforce = append(reinforce, fmt.Sprintf("%s = %s by %s %s", gap.English, gap.Spanish, gap.Route, gap.Thought))
		}
	}
	if len(recycle) > 0 {
		b.WriteString(" Words he reached for and didn't have, recycle them: " + strings.Join(recycle, "; ") + ".")
	}
	if len(reinforce) > 0 {
		b.WriteString(" Routes he had but didn't use, reinforce them: " + strings.Join(reinforce, "; ") + ".")
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
