package live

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Plan is one call's lesson: its mode and the items to work through, each marked review or new
// with the reason, so the mix can be read and tuned.
type Plan struct {
	Created     time.Time  `json:"created"`
	Mode        string     `json:"mode"`
	Why         string     `json:"why"`
	Items       []PlanItem `json:"items"`
	Teasers     []Teaser   `json:"teasers"`
	Slang       string     `json:"slang"`
	Etymologies []string   `json:"etymologies"`
}

// PlanItem is one step of the call. A thought item teaches or reviews a thought with fresh targets
// from Lemon's life; a life item asks him about his life and builds the Spanish from his answer; a
// listen item is a story from Rosa's canon he decodes by ear.
type PlanItem struct {
	Type     string     `json:"type"`
	Thought  string     `json:"thought"`
	Kind     string     `json:"kind"`
	Why      string     `json:"why"`
	Weave    []string   `json:"weave"`
	Targets  []Sentence `json:"targets"`
	Domain   string     `json:"domain"`
	Question string     `json:"question"`
	Topics   []string   `json:"topics"`
	Passage  Passage    `json:"passage"`
}

// Passage is a short story in slow Spanish, cut into chunks he decodes one by one.
type Passage struct {
	Canon  string  `json:"canon"`
	ES     string  `json:"es"`
	Gist   string  `json:"gist"`
	Chunks []Chunk `json:"chunks"`
}

// Chunk is a piece of a passage: its English, how he can reach it, and a ladder of hints, the English
// itself last.
type Chunk struct {
	ES    string   `json:"es"`
	EN    string   `json:"en"`
	Route string   `json:"route"`
	Hints []string `json:"hints"`
}

// Teaser is a slip from Rosa's canon for this call.
type Teaser struct {
	Canon string `json:"canon"`
	Line  string `json:"line"`
}

var planFormat = json.RawMessage(`{"type":"json_schema","name":"plan","strict":true,"schema":{"type":"object","properties":{
"mode":{"type":"string","enum":["teach","mixed","talk"]},
"why":{"type":"string","description":"One or two sentences: why this mode and this mix of review and new ground."},
"teasers":{"type":"array","description":"Two to four slips from her canon for this call, the first early: one clause or sentence each.","items":{"type":"object","properties":{"canon":{"type":"string"},"line":{"type":"string"}},"required":["canon","line"],"additionalProperties":false}},
"etymologies":{"type":"array","items":{"type":"string"},"description":"One to three etymology stories for the dips, each about a word in this call, each checked with the etymology tool and saying what it rests on."},
"slang":{"type":"string","description":"At most one new slang, swear or sex word for this call, with how it comes up in a slip or story; or empty."},
"items":{"type":"array","items":{"type":"object","properties":{
"type":{"type":"string","enum":["thought","life","listen"]},
"thought":{"type":"string","description":"Thought id from the graph; for life and listen items, the thought the item practises most, or empty."},
"kind":{"type":"string","enum":["review","new"]},
"domain":{"type":"string","description":"Life items: the life domain asked about. Otherwise empty."},
"question":{"type":"string","description":"Life items: the question Rosa asks him in English. Otherwise empty."},
"topics":{"type":"array","items":{"type":"string"},"description":"Topics the item's sentences or story are about, for the topic log."},
"passage":{"type":"object","description":"Listen items: the story. Otherwise empty strings and no chunks.","properties":{"canon":{"type":"string"},"es":{"type":"string"},"gist":{"type":"string"},"chunks":{"type":"array","items":{"type":"object","properties":{"es":{"type":"string"},"en":{"type":"string"},"route":{"type":"string"},"hints":{"type":"array","items":{"type":"string"}}},"required":["es","en","route","hints"],"additionalProperties":false}}},"required":["canon","es","gist","chunks"],"additionalProperties":false},
"why":{"type":"string","description":"Why this thought now: due, an active error, found only with help, next unlocked, or asked for."},
"weave":{"type":"array","items":{"type":"string"},"description":"Ids of due thoughts hidden inside this item's targets."},
"targets":{"type":"array","items":{"type":"object","properties":{
"en":{"type":"string","description":"The English cue Rosa asks."},
"es":{"type":"string","description":"The expected Spanish, lowercase, accents kept, no final punctuation; vos where it applies."},
"also":{"type":"array","items":{"type":"string"},"description":"Other answers that are also right, such as the tú form."},
"note":{"type":"string","description":"Feedback to give when he gets it, or empty."},
"misses":{"type":"array","items":{"type":"object","properties":{"said":{"type":"string"},"cause":{"type":"string"},"ask":{"type":"string"}},"required":["said","cause","ask"],"additionalProperties":false}},
"words":{"type":"array","description":"Every word of es not in his dictionary, marked guessable or not.","items":{"type":"object","properties":{"word":{"type":"string"},"guessable":{"type":"boolean"},"route":{"type":"string","description":"How he can reach it: a conversion rule id, cognate, shared root, Norwegian; or empty."},"hint":{"type":"string","description":"Points at the route without giving the word, or empty."}},"required":["word","guessable","route","hint"],"additionalProperties":false}}},
"required":["en","es","also","note","misses","words"],"additionalProperties":false}}},
"required":["type","thought","kind","domain","question","topics","passage","why","weave","targets"],"additionalProperties":false}}},
"required":["mode","why","teasers","slang","etymologies","items"],"additionalProperties":false}}`)

func loadPlan(path string) (Plan, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, false
	}
	var plan Plan
	if json.Unmarshal(data, &plan) != nil || plan.Mode == "" {
		return Plan{}, false
	}
	return plan, true
}

const (
	composeNext   = "Compose the plan for Lemon's next call."
	composeStart  = "Compose the plan for the call that is starting now."
	composeMore   = "This call's plan has run out and Lemon is still going. Compose more items for the rest of this call, picking up from where it is now: mostly new ground in bigger steps (combine thoughts he owns into longer sentences), a life question or a listening story among them, and never a thought this call already worked on twice; its weakness goes to the next call. Don't repeat targets already asked in it."
	composeHarder = "Lemon says this is too easy. Jump ahead to where he really is: first a quick placement probe, one item of three or four fresh sentences drawn from later thoughts in curriculum order, each harder than the last, then new items from the furthest point he can likely handle. Nothing he has found alone, no review."
)

func savePlan(path string, plan Plan) error {
	data, _ := json.MarshalIndent(plan, "", "  ")
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// Overview is the plan as the live model's first notes of the call.
func (p Plan) Overview(graph *Graph) []string {
	var b strings.Builder
	fmt.Fprintf(&b, "This call's plan. Mode: %s. %s", p.Mode, p.Why)
	for i, item := range p.Items {
		title := item.Thought
		if thought, ok := graph.Get(item.Thought); ok {
			title = thought.Title
		}
		switch item.Type {
		case "life":
			title = "life question about " + item.Domain
		case "listen":
			title = "listening: a story from your life"
		}
		fmt.Fprintf(&b, "\n%d. [%s] %s: %s", i+1, item.Kind, title, item.Why)
	}
	for _, teaser := range p.Teasers {
		fmt.Fprintf(&b, "\nTeaser to slip in (one clause, never while a question waits): %s", teaser.Line)
	}
	for _, story := range p.Etymologies {
		b.WriteString("\nEtymology for a dip, verified: " + story)
	}
	if p.Slang != "" {
		b.WriteString("\nNew word for a slip or story this call: " + p.Slang)
	}
	if len(p.Items) == 0 {
		b.WriteString("\nNo teaching items: just talk with him in Spanish at his level, and teach what his errors call for.")
	}
	return chunk(strings.Split(b.String(), "\n"), updateLimit)
}

// Candidates is what the planner chooses from: level, thoughts due for review, active errors, the
// unlocked frontier in curriculum order, and the wording log.
func Candidates(graph *Graph, learner *LearnerStore, now time.Time) string {
	learner.mu.Lock()
	l := learner.data
	statuses := map[string]ThoughtStatus{}
	for id, status := range l.Thoughts {
		statuses[id] = status
	}
	errors := tail(append([]LearnerError(nil), l.Errors...), 10)
	used := tail(append([]string(nil), l.Used...), 60)
	var dictionary []string
	for _, word := range l.Words {
		switch {
		case word.Gender != "":
			dictionary = append(dictionary, word.Gender+" "+word.Word)
		case word.Kind == "endings" || word.Kind == "form":
			dictionary = append(dictionary, word.Word+" ("+word.Kind+", memorised)")
		default:
			dictionary = append(dictionary, word.Word)
		}
	}
	calls := len(l.Calls)
	learner.mu.Unlock()

	all := graph.All()
	alone, helped := 0, 0
	for _, status := range statuses {
		switch status.Status {
		case "found alone":
			alone++
		case "found with help":
			helped++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Level: %d calls so far. Of %d thoughts in the graph, %d found alone, %d found with help, %d introduced only.", calls, len(all), alone, helped, len(statuses)-alone-helped)

	erring := map[string]bool{}
	for _, e := range errors {
		erring[e.Thought] = true
	}
	type review struct {
		thought Thought
		status  ThoughtStatus
	}
	var due []review
	for _, thought := range all {
		if status, ok := statuses[thought.ID]; ok && status.Due(now) {
			due = append(due, review{thought, status})
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if erring[due[i].thought.ID] != erring[due[j].thought.ID] {
			return erring[due[i].thought.ID]
		}
		return due[i].status.At.Before(due[j].status.At)
	})
	b.WriteString("\nDue for review (active errors first, then longest unseen):")
	if len(due) == 0 {
		b.WriteString(" none.")
	}
	for _, r := range due[:min(len(due), 15)] {
		fmt.Fprintf(&b, "\n- %s %q: %s, seen %d times, found alone %d, last %s", r.thought.ID, r.thought.Title, r.status.Status, r.status.Seen, r.status.Alone, ago(now.Sub(r.status.At)))
	}
	b.WriteString("\nActive errors:")
	if len(errors) == 0 {
		b.WriteString(" none yet.")
	}
	for _, e := range errors {
		fmt.Fprintf(&b, "\n- [%s] said %q for %q: %s (%s)", e.Thought, e.Said, e.Expected, e.Cause, ago(now.Sub(e.At)))
	}
	b.WriteString("\nNext in curriculum order; each needs only thoughts he has found or ones above it here, so a plan can take several in order (habits are cued in the call, not planned):")
	next := Frontier(graph, statuses, 12)
	for _, thought := range next {
		fmt.Fprintf(&b, "\n- %s %q (%s, %s)", thought.ID, thought.Title, thought.Kind, thought.Source)
	}
	frontier := len(next)
	if frontier == 0 {
		b.WriteString(" nothing left unlocked. Author new thoughts for what his Spanish and his life need next.")
	}
	if len(used) > 0 {
		b.WriteString("\nWording log, never reuse these: " + strings.Join(used, "; "))
	}
	known := map[string]int{}
	for _, fact := range l.Facts {
		known[fact.Domain]++
	}
	var topics []string
	for _, use := range tail(append([]TopicUse(nil), l.Topics...), 20) {
		topics = append(topics, use.Topic)
	}
	queue := append([]string(nil), Domains...)
	sort.SliceStable(queue, func(i, j int) bool { return known[queue[i]] < known[queue[j]] })
	b.WriteString("\nLife domains to ask about, least known first: " + strings.Join(queue[:6], ", ") + ".")
	if len(topics) > 0 {
		b.WriteString(" Topics used in recent calls, don't repeat them: " + strings.Join(topics, ", ") + ".")
	}
	sort.Strings(dictionary)
	b.WriteString("\nHis dictionary, every Spanish word he has met: " + strings.Join(dictionary, ", "))
	return b.String()
}

// Frontier is the next n thoughts in curriculum order that he hasn't met, each needing only thoughts he
// has found or ones before it in the list. Habits are cued in the call, never planned.
func Frontier(graph *Graph, statuses map[string]ThoughtStatus, n int) []Thought {
	listed := map[string]bool{}
	var next []Thought
	for _, thought := range graph.All() {
		if _, seen := statuses[thought.ID]; seen || thought.Kind == "habit" {
			continue
		}
		open := true
		for _, need := range thought.Needs {
			if prerequisite, _ := graph.Get(need); prerequisite.Kind == "habit" || listed[need] {
				continue
			}
			if s := statuses[need].Status; s != "found alone" && s != "found with help" {
				open = false
				break
			}
		}
		if open {
			listed[thought.ID] = true
			if next = append(next, thought); len(next) == n {
				break
			}
		}
	}
	return next
}

// compose asks the planner for a plan: for the next call after one ends, for this call when no plan
// is waiting, or more items when this call's plan runs out. transcript is the call so far, or empty.
func (r *Rosa) compose(ctx context.Context, backend *Backend, task, transcript string, now time.Time) (Plan, error) {
	graph, err := r.Graph()
	if err != nil {
		return Plan{}, err
	}
	input := []map[string]any{
		seedMessage("developer", "It is "+clock(now)+". "+r.learner.Summary(now)),
		seedMessage("developer", Candidates(graph, r.learner, now)),
		seedMessage("developer", truncate(r.canon.Summary(now), 12000)),
	}
	if passages, err := r.read("passages.json"); err == nil {
		input = append(input, seedMessage("developer", "Example listening passages, for format and level, not to reuse as they are: "+truncate(passages, 8000)))
	}
	if transcript != "" {
		input = append(input, seedMessage("developer", "The call:\n"+truncate(transcript, 40000)))
	}
	input = append(input, seedMessage("developer", task+" Follow the planning rules. Read each chosen thought with the thought tool before writing its targets. Author a thought first when the next thing he needs is not in the graph."))
	return r.plan(ctx, backend, input, now)
}

// adjustPlan rewrites the plan's items from index from onward to fit what Lemon said today.
func (r *Rosa) adjustPlan(ctx context.Context, backend *Backend, plan Plan, from int, transcript string, now time.Time) (Plan, error) {
	rest, _ := json.Marshal(Plan{Mode: plan.Mode, Why: plan.Why, Items: plan.Items[from:]})
	input := []map[string]any{
		seedMessage("developer", "It is "+clock(now)+". "+r.learner.Summary(now)),
		seedMessage("developer", "Today's call so far:\n"+truncate(transcript, 20000)),
		seedMessage("developer", "The rest of today's plan, not yet taught:\n"+string(rest)),
		seedMessage("developer", "Adjust the rest of the plan to what Lemon said and asked for today: rebuild targets from what he told you, add or swap an item he asked for, switch mode if he wants to just talk. Keep the review and new mix unless today gives a reason. Return the adjusted rest of the plan."),
	}
	return r.plan(ctx, backend, input, now)
}

func (r *Rosa) plan(ctx context.Context, backend *Backend, input []map[string]any, now time.Time) (Plan, error) {
	text, _, err := backend.Complete(ctx, input, planFormat, 24)
	if err != nil {
		return Plan{}, err
	}
	var plan Plan
	if err := json.Unmarshal([]byte(text), &plan); err != nil {
		return Plan{}, fmt.Errorf("the planner returned no plan")
	}
	graph, err := r.Graph()
	if err != nil {
		return Plan{}, err
	}
	plan.Created = now
	kept := plan.Items[:0]
	for _, item := range plan.Items {
		var targets []Sentence
		for _, target := range item.Targets {
			if strings.TrimSpace(target.EN) == "" || strings.TrimSpace(target.ES) == "" {
				continue
			}
			var fresh []Fresh
			for _, word := range target.Words {
				if !r.learner.Known(word.Word) {
					fresh = append(fresh, word)
				}
			}
			target.Words = fresh
			targets = append(targets, target)
		}
		item.Targets = targets
		_, known := graph.Get(item.Thought)
		switch item.Type {
		case "life":
			if item.Question != "" {
				kept = append(kept, item)
			}
		case "listen":
			if item.Passage.ES != "" && len(item.Passage.Chunks) > 0 {
				kept = append(kept, item)
			}
		default:
			if item.Type = "thought"; known && len(targets) > 0 {
				kept = append(kept, item)
			}
		}
	}
	plan.Items = kept
	return plan, nil
}
