package live

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Thought is one node of Rosa's thought graph: one idea taught on its own, what it needs first, ways
// to explain and reframe it, example targets, and the near misses to expect.
type Thought struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Kind     string     `json:"kind"`
	Source   string     `json:"source"`
	Needs    []string   `json:"needs"`
	Teach    []string   `json:"teach"`
	Reframes []string   `json:"reframes,omitempty"`
	Import   string     `json:"import,omitempty"`
	Links    []string   `json:"links,omitempty"`
	Examples []Sentence `json:"examples,omitempty"`
	Misses   []Miss     `json:"misses,omitempty"`
	Aside    string     `json:"aside,omitempty"`
}

// Sentence is one target sentence: the English cue Rosa asks and the Spanish she expects.
type Sentence struct {
	EN     string   `json:"en"`
	ES     string   `json:"es"`
	Also   []string `json:"also,omitempty"`
	Note   string   `json:"note,omitempty"`
	Misses []Miss   `json:"misses,omitempty"`
	Words  []Fresh  `json:"words,omitempty"`
}

// Fresh is a word in a target that is not in Lemon's dictionary: guessable from English or
// Norwegian by its route, with a hint, or to be given.
type Fresh struct {
	Word      string `json:"word"`
	Guessable bool   `json:"guessable"`
	Route     string `json:"route"`
	Hint      string `json:"hint"`
}

// Miss is a known wrong answer, its cause, and the question that leads Lemon to the fix.
type Miss struct {
	Said  string `json:"said"`
	Cause string `json:"cause"`
	Ask   string `json:"ask"`
}

// Graph is the seed thoughts from dotagents in curriculum order, then the ones Rosa authored.
type Graph struct {
	mu       sync.Mutex
	authored string
	thoughts []Thought
	index    map[string]int
}

// LoadGraph reads every seed *.json array in dir, in file name order, then authored, one thought
// per line.
func LoadGraph(dir, authored string) (*Graph, error) {
	g := &Graph{authored: authored, index: map[string]int{}}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(paths)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var thoughts []Thought
		if err := json.Unmarshal(data, &thoughts); err != nil {
			return nil, fmt.Errorf("%s: %v", filepath.Base(path), err)
		}
		for _, thought := range thoughts {
			g.put(thought)
		}
	}
	if data, err := os.ReadFile(authored); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			var thought Thought
			if strings.TrimSpace(line) != "" && json.Unmarshal([]byte(line), &thought) == nil {
				g.put(thought)
			}
		}
	}
	if len(g.thoughts) == 0 {
		return nil, fmt.Errorf("the thought graph in %s is empty", dir)
	}
	return g, nil
}

func (g *Graph) put(thought Thought) {
	if thought.ID == "" {
		return
	}
	if i, ok := g.index[thought.ID]; ok {
		g.thoughts[i] = thought
		return
	}
	g.index[thought.ID] = len(g.thoughts)
	g.thoughts = append(g.thoughts, thought)
}

func (g *Graph) Get(id string) (Thought, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	i, ok := g.index[id]
	if !ok {
		return Thought{}, false
	}
	return g.thoughts[i], true
}

// All is every thought in curriculum order.
func (g *Graph) All() []Thought {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Thought(nil), g.thoughts...)
}

// Author saves a new thought with source rosa. Its needs must already be in the graph.
func (g *Graph) Author(thought Thought) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	thought.ID = strings.TrimSpace(thought.ID)
	if thought.ID == "" || thought.Title == "" {
		return fmt.Errorf("a thought needs an id and a title")
	}
	if _, ok := g.index[thought.ID]; ok {
		return fmt.Errorf("thought %s already exists; pick another id", thought.ID)
	}
	for _, need := range thought.Needs {
		if _, ok := g.index[need]; !ok {
			return fmt.Errorf("unknown prerequisite %s", need)
		}
	}
	thought.Source = "rosa"
	data, _ := json.Marshal(thought)
	os.MkdirAll(filepath.Dir(g.authored), 0o700)
	file, err := os.OpenFile(g.authored, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	g.put(thought)
	return nil
}

// Find lists thoughts whose id, title, kind or teach wordings contain every word of query.
func (g *Graph) Find(query string, limit int) []map[string]any {
	terms := words(query)
	var found []map[string]any
	for _, thought := range g.All() {
		text := " " + strings.Join(words(thought.ID+" "+thought.Title+" "+thought.Kind+" "+strings.Join(thought.Teach, " ")), " ") + " "
		match := true
		for _, term := range terms {
			if !strings.Contains(text, term) {
				match = false
				break
			}
		}
		if match {
			found = append(found, map[string]any{"id": thought.ID, "title": thought.Title, "kind": thought.Kind, "needs": thought.Needs})
			if len(found) == limit {
				break
			}
		}
	}
	return found
}

// Notes renders a plan item as quiet context for the live model, in appends that fit its limit.
func (item PlanItem) Notes(thought Thought, position string) []string {
	lines := []string{fmt.Sprintf("Plan item %s, %s: %s [%s]. Why: %s", position, item.Kind, thought.Title, thought.ID, item.Why)}
	if item.Kind == "review" {
		lines = append(lines, "He has met this before. Don't re-teach it; let the sentences bring it back, and explain only if he stumbles.")
	}
	if len(thought.Teach) > 0 {
		lines = append(lines, "Ways to explain it; use one, in your own words, and change the wording when you come back to it:")
		for _, teach := range thought.Teach {
			lines = append(lines, "- "+teach)
		}
	}
	for _, reframe := range thought.Reframes {
		lines = append(lines, "Reframe: "+reframe)
	}
	if thought.Import != "" {
		lines = append(lines, "From Norwegian: "+thought.Import)
	}
	for _, link := range thought.Links {
		lines = append(lines, "Link: "+link)
	}
	lines = append(lines, "Target sentences in this order. Ask each in English, then wait for his whole answer:")
	for i, sentence := range item.Targets {
		line := fmt.Sprintf("%d. %q → %s", i+1, sentence.EN, sentence.ES)
		if len(sentence.Also) > 0 {
			line += " (also right: " + strings.Join(sentence.Also, "; ") + ")"
		}
		if sentence.Note != "" {
			line += " Feedback: " + sentence.Note
		}
		for _, miss := range sentence.Misses {
			line += fmt.Sprintf(" Near miss %q: %s Ask: %s", miss.Said, miss.Cause, miss.Ask)
		}
		for _, fresh := range sentence.Words {
			if fresh.Guessable {
				line += fmt.Sprintf(" New word %s: guessable by %s; challenge him, hint: %s.", fresh.Word, fresh.Route, fresh.Hint)
			} else {
				line += fmt.Sprintf(" New word %s: give it.", fresh.Word)
			}
		}
		lines = append(lines, line)
	}
	if len(thought.Misses) > 0 {
		lines = append(lines, "Known near misses for this thought:")
		for _, miss := range thought.Misses {
			lines = append(lines, fmt.Sprintf("- %q: %s Ask: %s", miss.Said, miss.Cause, miss.Ask))
		}
	}
	if thought.Aside != "" {
		lines = append(lines, "Aside for when the load needs a breather: "+thought.Aside)
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
