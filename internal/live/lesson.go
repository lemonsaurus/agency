package live

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Lesson is one hand-built Thinking Method lesson: thoughts in teaching order, each with its
// explanation wordings, target sentences, and the near misses real learners produce.
type Lesson struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Source   string    `json:"source"`
	Mission  string    `json:"mission"`
	Close    string    `json:"close"`
	Thoughts []Thought `json:"thoughts"`
}

// Thought is one idea taught on its own: what it needs first, ways to explain it, the sentences
// that practise it, and the misses to expect.
type Thought struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Needs     []string   `json:"needs"`
	Teach     []string   `json:"teach"`
	Import    string     `json:"import,omitempty"`
	Sentences []Sentence `json:"sentences"`
	Misses    []Miss     `json:"misses"`
	Aside     string     `json:"aside,omitempty"`
}

// Sentence is one target sentence: the English cue Rosa asks and the Spanish she expects.
type Sentence struct {
	EN   string   `json:"en"`
	ES   string   `json:"es"`
	Also []string `json:"also,omitempty"`
	Note string   `json:"note,omitempty"`
}

// Miss is a known wrong answer, its cause, and the question that leads Lemon to the fix.
type Miss struct {
	Said  string `json:"said"`
	Cause string `json:"cause"`
	Ask   string `json:"ask"`
}

// LoadLessons reads every lesson-*.json in dir, in file name order.
func LoadLessons(dir string) ([]Lesson, error) {
	paths, _ := filepath.Glob(filepath.Join(dir, "lesson-*.json"))
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("no lessons in %s", dir)
	}
	var lessons []Lesson
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var lesson Lesson
		if err := json.Unmarshal(data, &lesson); err != nil {
			return nil, fmt.Errorf("%s: %v", filepath.Base(path), err)
		}
		if len(lesson.Thoughts) == 0 {
			return nil, fmt.Errorf("%s has no thoughts", filepath.Base(path))
		}
		lessons = append(lessons, lesson)
	}
	return lessons, nil
}

// Overview is the lesson in one line per thought, with Lemon's status for each.
func (l Lesson) Overview(status func(id string) string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Lesson %s, %q (%s). Mission: %s\nThoughts in order:", l.ID, l.Title, l.Source, l.Mission)
	for i, thought := range l.Thoughts {
		fmt.Fprintf(&b, "\n%d. %s [%s]: %s", i+1, thought.Title, thought.ID, status(thought.ID))
	}
	return b.String()
}

// Notes renders a thought as quiet context for the live model, in appends that fit its limit.
func (t Thought) Notes(position string) []string {
	lines := []string{fmt.Sprintf("Next thought, %s: %s [%s].", position, t.Title, t.ID)}
	if len(t.Needs) > 0 {
		lines = append(lines, "Builds on: "+strings.Join(t.Needs, ", ")+".")
	}
	if len(t.Teach) > 0 {
		lines = append(lines, "Ways to explain it; use one, in your own words, and change the wording when you come back to it:")
		for _, teach := range t.Teach {
			lines = append(lines, "- "+teach)
		}
	}
	if t.Import != "" {
		lines = append(lines, "From Norwegian: "+t.Import)
	}
	lines = append(lines, "Target sentences in this order. Ask each in English, then wait for his whole answer:")
	for i, sentence := range t.Sentences {
		line := fmt.Sprintf("%d. %q → %s", i+1, sentence.EN, sentence.ES)
		if len(sentence.Also) > 0 {
			line += " (also right: " + strings.Join(sentence.Also, "; ") + ")"
		}
		if sentence.Note != "" {
			line += " Feedback: " + sentence.Note
		}
		lines = append(lines, line)
	}
	if len(t.Misses) > 0 {
		lines = append(lines, "Known near misses for this thought:")
		for _, miss := range t.Misses {
			lines = append(lines, fmt.Sprintf("- %q: %s Ask: %s", miss.Said, miss.Cause, miss.Ask))
		}
	}
	if t.Aside != "" {
		lines = append(lines, "Aside for when the load needs a breather: "+t.Aside)
	}
	return chunk(lines, updateLimit)
}

// chunk joins lines into pieces no longer than limit runes, splitting only between lines.
func chunk(lines []string, limit int) []string {
	var pieces []string
	var current strings.Builder
	for _, line := range lines {
		line = truncate(line, limit)
		if current.Len() > 0 && len([]rune(current.String()))+1+len([]rune(line)) > limit {
			pieces = append(pieces, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteByte('\n')
		}
		current.WriteString(line)
	}
	if current.Len() > 0 {
		pieces = append(pieces, current.String())
	}
	return pieces
}
