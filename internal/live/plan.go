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
	Created time.Time  `json:"created"`
	Mode    string     `json:"mode"`
	Why     string     `json:"why"`
	Items   []PlanItem `json:"items"`
}

// PlanItem is one thought for the call with fresh targets built from Lemon's life.
type PlanItem struct {
	Thought string     `json:"thought"`
	Kind    string     `json:"kind"`
	Why     string     `json:"why"`
	Weave   []string   `json:"weave"`
	Targets []Sentence `json:"targets"`
}

var planFormat = json.RawMessage(`{"type":"json_schema","name":"plan","strict":true,"schema":{"type":"object","properties":{
"mode":{"type":"string","enum":["teach","mixed","talk"]},
"why":{"type":"string","description":"One or two sentences: why this mode and this mix of review and new ground."},
"items":{"type":"array","items":{"type":"object","properties":{
"thought":{"type":"string","description":"Thought id from the graph."},
"kind":{"type":"string","enum":["review","new"]},
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
"required":["thought","kind","why","weave","targets"],"additionalProperties":false}}},
"required":["mode","why","items"],"additionalProperties":false}}`)

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

func savePlan(path string, plan Plan) error {
	data, _ := json.MarshalIndent(plan, "", "  ")
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// Overview is the plan as the live model's first note of the call.
func (p Plan) Overview(graph *Graph) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This call's plan. Mode: %s. %s", p.Mode, p.Why)
	for i, item := range p.Items {
		title := item.Thought
		if thought, ok := graph.Get(item.Thought); ok {
			title = thought.Title
		}
		fmt.Fprintf(&b, "\n%d. [%s] %s: %s", i+1, item.Kind, title, item.Why)
	}
	if len(p.Items) == 0 {
		b.WriteString("\nNo teaching items: just talk with him in Spanish at his level, and teach what his errors call for.")
	}
	return truncate(b.String(), updateLimit)
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
	listed := map[string]bool{}
	frontier := 0
	for _, thought := range all {
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
			fmt.Fprintf(&b, "\n- %s %q (%s, %s)", thought.ID, thought.Title, thought.Kind, thought.Source)
			if frontier++; frontier == 12 {
				break
			}
		}
	}
	if frontier == 0 {
		b.WriteString(" nothing left unlocked. Author new thoughts for what his Spanish and his life need next.")
	}
	if len(used) > 0 {
		b.WriteString("\nWording log, never reuse these: " + strings.Join(used, "; "))
	}
	sort.Strings(dictionary)
	b.WriteString("\nHis dictionary, every Spanish word he has met: " + strings.Join(dictionary, ", "))
	return b.String()
}

// compose asks the planner for a call plan. transcript is the call that just ended, or empty.
func (r *Rosa) compose(ctx context.Context, backend *Backend, transcript string, now time.Time) (Plan, error) {
	graph, err := r.Graph()
	if err != nil {
		return Plan{}, err
	}
	input := []map[string]any{
		seedMessage("developer", "It is "+clock(now)+". "+r.learner.Summary(now)),
		seedMessage("developer", Candidates(graph, r.learner, now)),
	}
	if transcript != "" {
		input = append(input, seedMessage("developer", "The call that just ended:\n"+truncate(transcript, 40000)))
	}
	input = append(input, seedMessage("developer", "Compose the plan for Lemon's next call, following the planning rules. Read each chosen thought with the thought tool before writing its targets. Author a thought first when the next thing he needs is not in the graph."))
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
		if _, ok := graph.Get(item.Thought); ok && len(targets) > 0 {
			kept = append(kept, item)
		}
	}
	plan.Items = kept
	return plan, nil
}
