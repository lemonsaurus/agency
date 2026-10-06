package live

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// Rosa may speak over a partial answer only after this much silence from Lemon.
	stallLimit = 45 * time.Second
	// Late transcript fragments can still complete an answer when Rosa starts speaking.
	settleDelay = 700 * time.Millisecond
	// A code-switched turn is sent for gap help after this much silence.
	switchPause = 1500 * time.Millisecond
	// A target's English cue arms it when it ends within this many words of the end of Rosa's turn.
	cueWindow = 10
	holdFloor = "Lemon is still building his answer and has only said part of it. Stop talking now. Stay silent and keep listening until he finishes it, says he doesn't know, or asks you something. No hints."
)

// Gap is one code-switched turn worked out: what he meant, and each English chunk with its Spanish
// and the route to it.
type Gap struct {
	Said     string     `json:"said"`
	Sentence string     `json:"sentence"`
	Chunks   []GapChunk `json:"chunks"`
	Aside    string     `json:"aside"`
}

// GapChunk is one English chunk: its Spanish, how he could reach it, and the question to elicit it.
type GapChunk struct {
	English   string   `json:"english"`
	Spanish   string   `json:"spanish"`
	Also      []string `json:"also"`
	Route     string   `json:"route"`
	Thought   string   `json:"thought"`
	Guessable bool     `json:"guessable"`
	Hint      string   `json:"hint"`
	Ask       string   `json:"ask"`
	Link      string   `json:"link"`
	Misses    []Miss   `json:"misses"`
	Known     bool     `json:"-"`
}

// Class runs one lesson call: it hands the live model the plan one item at a time, referees the floor
// after each target-sentence prompt, works out code-switched turns, logs every floor event, and
// records the outcome.
type Class struct {
	mu      sync.Mutex
	graph   *Graph
	learner *LearnerStore
	logPath string
	started time.Time
	outbox  chan update
	// onSwitch asks for gap help on a code-switched turn; prompted fires once, at the first target;
	// exhausted asks for more items when the last planned target is asked.
	onSwitch  func(said, context string)
	prompted  func()
	exhausted func()

	plan      Plan
	planned   bool
	current   int          // item being taught
	handed    map[int]bool // items whose notes reached the call
	extending bool         // more items are being composed for this call
	outcomes  map[string]string
	misses    map[string]int
	touched   map[string]bool
	gap       *Gap
	gapID     int
	adhoc     Sentence // a target Rosa improvised, refereed by length only
	asked     string   // the turn last sent for gap help

	floor      string  // who holds the floor: rosa or lemon
	turn       string  // Rosa's words since Lemon last spoke
	armed      *[2]int // item and target index being worked on; item -1 is the gap exchange
	fresh      bool    // Rosa's last turn ended on that prompt, so the floor is his
	attempt    string  // Lemon's words since the prompt
	attemptID  int
	verdict    string
	held       bool // Rosa spoke over a partial answer; his next words continue it
	intervened bool
	rosaEnd    int64
	lemonEnd   int64
	lemonAt    time.Time
	thinkMS    int64
	stops      int
	switchAt   *time.Timer
}

type update struct{ kind, content string }

func newClass(graph *Graph, learner *LearnerStore, logPath string, now time.Time) *Class {
	return &Class{graph: graph, learner: learner, logPath: logPath, started: now, outbox: make(chan update, 64),
		handed: map[int]bool{}, outcomes: map[string]string{}, misses: map[string]int{}, touched: map[string]bool{}}
}

// run delivers queued appends to the session in order, outside the class lock.
func (c *Class) run(s *Session) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case u := <-c.outbox:
			if u.kind == "session.instructions.append" {
				s.Instruct(u.content)
			} else {
				s.append(u.kind, "", u.content)
			}
		}
	}
}

func (c *Class) send(kind, content string) {
	select {
	case c.outbox <- update{kind, content}:
	default:
	}
}

// SetPlan hands over the plan's overview and its first item.
func (c *Class) SetPlan(plan Plan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plan, c.planned = plan, true
	c.send("session.thinking.append", plan.Overview(c.graph))
	c.record("plan", map[string]any{"mode": plan.Mode, "why": plan.Why, "items": plan.Items})
	if len(plan.Items) > 0 {
		c.handOver(0)
	}
}

// Replace swaps the plan's items from index from onward, unless they already reached the call.
func (c *Class) Replace(from int, items []PlanItem, mode, why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if from > len(c.plan.Items) || c.handed[from] {
		return
	}
	c.plan.Items = append(c.plan.Items[:from:from], items...)
	c.plan.Mode, c.plan.Why = mode, why
	c.record("adjusted", map[string]any{"from": from, "items": len(items), "mode": mode})
	if mode == "talk" {
		c.send("session.thinking.append", "Adjusted plan: mostly conversation from here. "+why)
	}
}

// Teach puts an item in the call right after the current one and hands it over now.
func (c *Class) Teach(item PlanItem) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.current + 1
	if !c.planned || len(c.plan.Items) == 0 {
		at = 0
	}
	c.plan.Items = append(c.plan.Items[:at:at], append([]PlanItem{item}, c.plan.Items[at:]...)...)
	handed := map[int]bool{}
	for i := range c.handed {
		if i >= at {
			i++
		}
		handed[i] = true
	}
	c.handed, c.planned = handed, true
	if c.armed != nil && c.armed[0] >= at {
		c.armed = &[2]int{c.armed[0] + 1, c.armed[1]}
	}
	c.handOver(at)
	return "It is in the call's notes now; Rosa teaches it next."
}

// Talk switches the rest of the call to conversation.
func (c *Class) Talk() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	last := c.current
	for i := range c.handed {
		last = max(last, i)
	}
	if last+1 < len(c.plan.Items) {
		c.plan.Items = c.plan.Items[:last+1]
	}
	c.plan.Mode = "talk"
	c.record("talk", nil)
	return "The rest of the call is conversation. Teach only what his errors call for."
}

// Extend appends items composed during the call and hands over the first.
func (c *Class) Extend(items []PlanItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.extending = false
	if len(items) == 0 {
		return
	}
	at := len(c.plan.Items)
	c.plan.Items = append(c.plan.Items, items...)
	c.record("extended", map[string]any{"items": items})
	c.handOver(at)
}

// Unhanded is the index of the first plan item that has not reached the call.
func (c *Class) Unhanded() (int, Plan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := 0
	for c.handed[i] {
		i++
	}
	return i, c.plan
}

// handOver puts a plan item's notes into the live model's context.
func (c *Class) handOver(i int) {
	item := c.plan.Items[i]
	thought, ok := c.graph.Get(item.Thought)
	if !ok {
		thought = Thought{ID: item.Thought, Title: item.Thought}
	}
	for _, note := range item.Notes(thought, fmt.Sprintf("%d of %d", i+1, len(c.plan.Items))) {
		c.send("session.thinking.append", note)
	}
	c.handed[i] = true
	c.record("item", map[string]any{"item": i, "thought": item.Thought, "kind": item.Kind})
}

// watch is the session's transcript hook.
func (c *Class) watch(kind, text string, startMS, endMS int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if startMS == 0 {
		startMS = time.Since(c.started).Milliseconds()
		endMS = startMS
	}
	switch kind {
	case "session.input_transcript.delta":
		c.heard(text, startMS, endMS)
	case "session.output_transcript.delta":
		c.said(text, startMS, endMS)
	case "session.delegation.created":
		c.record("delegation", map[string]any{"t": startMS})
	}
}

func (c *Class) heard(text string, startMS, endMS int64) {
	if c.floor != "lemon" && !c.held {
		c.fresh = c.cue(words(c.turn))
		c.attemptID++
		c.attempt, c.verdict, c.intervened = "", "", false
		c.thinkMS = 0
		if c.rosaEnd > 0 {
			c.thinkMS = startMS - c.rosaEnd
		}
		c.record("attempt", map[string]any{"t": startMS, "target": c.key(c.armed), "think_ms": c.thinkMS})
	}
	c.held = false
	c.floor, c.turn = "lemon", ""
	c.attempt += text
	c.lemonEnd, c.lemonAt = endMS, time.Now()
	if switched(c.attempt) && c.onSwitch != nil {
		if c.switchAt != nil {
			c.switchAt.Stop()
		}
		id, said := c.attemptID, c.attempt
		c.switchAt = time.AfterFunc(switchPause, func() { c.switchCheck(id, said) })
	}
	if c.armed == nil {
		return
	}
	target := c.target(*c.armed)
	verdict := judge(c.attempt, target)
	if verdict != c.verdict {
		c.verdict = verdict
		c.record("verdict", map[string]any{"t": endMS, "target": c.key(c.armed), "verdict": verdict, "said": truncate(c.attempt, 300)})
	}
	if verdict == verdictRight {
		outcome := "alone"
		if c.misses[c.key(c.armed)] > 0 {
			outcome = "helped"
		}
		c.resolve(*c.armed, outcome)
	}
}

// switchCheck sends a code-switched turn for gap help once he has paused, unless he is still
// building a target answer.
func (c *Class) switchCheck(id int, said string) {
	c.mu.Lock()
	if id != c.attemptID || said != c.attempt || said == c.asked || (c.armed != nil && c.verdict == verdictPartial) {
		c.mu.Unlock()
		return
	}
	c.asked = said
	context := "free conversation"
	if c.armed != nil {
		target := c.target(*c.armed)
		context = fmt.Sprintf("answering the target %q, expected %q", target.EN, target.ES)
	}
	c.record("switch", map[string]any{"said": truncate(said, 300)})
	onSwitch := c.onSwitch
	c.mu.Unlock()
	onSwitch(said, context)
}

// SetGap hands Rosa the worked-out gap and arms its chunks and the full sentence as micro-targets.
func (c *Class) SetGap(gap Gap) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(gap.Chunks) == 0 {
		return
	}
	c.closeGap()
	c.gapID++
	c.gap = &gap
	lines := []string{fmt.Sprintf("Gap help for his turn %q. He meant: %s", gap.Said, gap.Sentence)}
	for i, chunk := range gap.Chunks {
		line := fmt.Sprintf("%d. %q → %s, route: %s", i+1, chunk.English, chunk.Spanish, chunk.Route)
		if chunk.Thought != "" {
			line += " (" + chunk.Thought + ")"
		}
		switch {
		case chunk.Known:
			line += ". In his dictionary: challenge him to produce it: " + chunk.Ask
		case chunk.Guessable:
			line += ". New but guessable: challenge him (" + chunk.Ask + ") with the hint: " + chunk.Hint
		default:
			line += ". New and not guessable: give it, then he builds the sentence with it"
		}
		if chunk.Link != "" {
			line += ". Link: " + chunk.Link
		}
		for _, miss := range chunk.Misses {
			line += fmt.Sprintf(" Near miss %q: %s", miss.Said, miss.Cause)
		}
		lines = append(lines, line)
	}
	lines = append(lines, "In free talk where flow matters or when his load is high, you may give it all at once. Either way he ends by saying the whole sentence himself.")
	if gap.Aside != "" {
		lines = append(lines, "Aside: "+gap.Aside)
	}
	for _, note := range chunk(lines, updateLimit) {
		c.send("session.thinking.append", note)
	}
	c.record("gap", map[string]any{"said": truncate(gap.Said, 300), "sentence": gap.Sentence, "chunks": len(gap.Chunks)})
}

// closeGap files the finished gap exchange in learner memory: unfound words to recycle, unused routes
// to reinforce.
func (c *Class) closeGap() {
	if c.gap == nil {
		return
	}
	var records []GapRecord
	for i, chunk := range c.gap.Chunks {
		outcome := c.outcomes[c.key(&[2]int{-1, i})]
		records = append(records, GapRecord{At: time.Now(), English: chunk.English, Spanish: chunk.Spanish, Route: chunk.Route, Thought: chunk.Thought,
			Found: outcome == "alone" || outcome == "helped"})
	}
	c.learner.Update(func(l *Learner) {
		l.Gaps = append(l.Gaps, records...)
		for _, record := range records {
			how := "told"
			if record.Found {
				how = "found with help"
			}
			l.expose(record.Spanish, how, record.At)
		}
	})
	c.gap = nil
	if c.armed != nil && c.armed[0] < 0 {
		c.armed = nil
	}
}

func (c *Class) said(text string, startMS, endMS int64) {
	c.turn += text
	c.rosaEnd = endMS
	turn := words(c.turn)
	if c.floor == "lemon" && backchannel(turn) {
		return
	}
	if c.floor != "rosa" {
		c.floor = "rosa"
		c.record("rosa", map[string]any{"t": startMS, "gap_ms": startMS - c.lemonEnd, "verdict": c.verdict})
		if c.armed != nil && c.attempt != "" {
			switch c.verdict {
			case verdictPartial:
				if !c.fresh {
					break
				}
				c.held = true
				if !c.intervened && time.Since(c.lemonAt) < stallLimit {
					id := c.attemptID
					time.AfterFunc(settleDelay, func() { c.settle(id) })
				}
			case verdictAttempt, verdictUnsure:
				c.misses[c.key(c.armed)]++
			}
		}
	}
	if c.armed != nil && c.misses[c.key(c.armed)] > 0 && says(turn, c.target(*c.armed).ES) {
		c.resolve(*c.armed, "shown")
	}
}

// settle stops Rosa if Lemon's answer is still partial once late fragments have arrived.
func (c *Class) settle(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id != c.attemptID || c.verdict != verdictPartial || c.intervened {
		if c.verdict != verdictPartial {
			c.held = false
		}
		return
	}
	c.intervened = true
	c.stops++
	c.record("intervene", map[string]any{"target": c.key(c.armed), "said": truncate(c.attempt, 300), "rosa": truncate(c.turn, 300)})
	c.send("session.instructions.append", holdFloor)
}

// cue arms the open target whose English cue Rosa said last, within the last words of her turn,
// and reports whether one did: gap chunks first, then the plan items that reached the call, then the
// whole gap sentence once Rosa has worked a chunk or given one, then a prompt she improvised.
func (c *Class) cue(turn []string) bool {
	best, at := (*[2]int)(nil), -1
	consider := func(item, i int, en string) {
		if c.outcomes[c.key(&[2]int{item, i})] != "" {
			return
		}
		if end := cued(turn, en); end >= 0 && end >= len(turn)-1-cueWindow && end >= at {
			best, at = &[2]int{item, i}, end
		}
	}
	if c.gap != nil {
		for i, chunk := range c.gap.Chunks {
			consider(-1, i, chunk.English)
		}
	}
	if best == nil {
		for item := c.current; item < len(c.plan.Items); item++ {
			if !c.handed[item] {
				continue
			}
			for i, target := range c.plan.Items[item].Targets {
				consider(item, i, target.EN)
			}
		}
	}
	if best == nil && c.gap != nil && len(turn) > 0 {
		whole := [2]int{-1, len(c.gap.Chunks)}
		started := false
		for i, chunk := range c.gap.Chunks {
			if c.outcomes[c.key(&[2]int{-1, i})] != "" || says(turn, chunk.Spanish) {
				started = true
			}
		}
		if started && c.outcomes[c.key(&whole)] == "" {
			best = &whole
		}
	}
	if best == nil {
		prompt, ok := improvised(turn)
		if !ok {
			return false
		}
		c.adhoc = Sentence{EN: prompt}
		delete(c.misses, "adhoc")
		best = &[2]int{-2, 0}
	}
	if c.armed != nil && *c.armed == *best {
		return true
	}
	if best[0] > c.current {
		c.advance(best[0])
	}
	c.armed, c.verdict = best, ""
	c.record("prompt", map[string]any{"target": c.key(best), "en": c.target(*best).EN, "es": c.target(*best).ES})
	if c.prompted != nil {
		go c.prompted()
		c.prompted = nil
	}
	if best[0] < 0 {
		return true
	}
	c.touched[c.plan.Items[best[0]].Thought] = true
	if best[1] == len(c.plan.Items[best[0]].Targets)-1 {
		c.lookAhead()
	}
	return true
}

// lookAhead hands over the next item while the last target of the current one is asked; when the
// plan runs out it asks for more, and the call never closes on its own.
func (c *Class) lookAhead() {
	if next := c.current + 1; next < len(c.plan.Items) {
		if !c.handed[next] {
			c.handOver(next)
		}
		return
	}
	if c.extending || c.plan.Mode == "talk" || c.exhausted == nil {
		return
	}
	c.extending = true
	c.record("exhausted", nil)
	c.send("session.thinking.append", "These are the last planned targets; more material is being prepared. Keep going with him meanwhile: talk, or build sentences from what he tells you. Never wrap up or end the call yourself.")
	go c.exhausted()
}

// advance moves the call to item i; targets skipped on the way are marked skipped.
func (c *Class) advance(i int) {
	for item := c.current; item < i; item++ {
		if !c.touched[c.plan.Items[item].Thought] {
			continue
		}
		for t := range c.plan.Items[item].Targets {
			if key := c.key(&[2]int{item, t}); c.outcomes[key] == "" {
				c.outcomes[key] = "skipped"
			}
		}
	}
	c.current = i
}

func (c *Class) resolve(at [2]int, outcome string) {
	key := c.key(&at)
	c.outcomes[key] = outcome
	if at[0] >= 0 {
		for i := 0; i < at[1]; i++ {
			if earlier := c.key(&[2]int{at[0], i}); c.outcomes[earlier] == "" {
				c.outcomes[earlier] = "skipped"
			}
		}
	}
	c.armed, c.verdict, c.held = nil, "", false
	target := c.target(at)
	c.record("resolved", map[string]any{"target": key, "outcome": outcome, "think_ms": c.thinkMS})
	think := c.thinkMS
	how := map[string]string{"alone": "found alone", "helped": "found with help", "shown": "told"}[outcome]
	c.learner.Update(func(l *Learner) {
		l.expose(target.ES, how, time.Now())
		l.Used = append(l.Used, target.ES)
		l.Pacing = append(l.Pacing, Pace{At: time.Now(), Sentence: target.ES, ThinkMS: think, Outcome: outcome})
	})
}

// Position is where the call stands, for Rosa's backend.
func (c *Class) Position() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.planned {
		return "The plan for this call is still being composed."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Mode %s. ", c.plan.Mode)
	if c.current < len(c.plan.Items) {
		item := c.plan.Items[c.current]
		fmt.Fprintf(&b, "Teaching item %d of %d, [%s] %s.", c.current+1, len(c.plan.Items), item.Kind, item.Thought)
	}
	if c.armed != nil {
		fmt.Fprintf(&b, " Open target: %q → %s.", c.target(*c.armed).EN, c.target(*c.armed).ES)
	}
	if len(c.outcomes) > 0 {
		b.WriteString(" Outcomes so far:")
		for i, item := range c.plan.Items {
			for t, target := range item.Targets {
				if outcome := c.outcomes[c.key(&[2]int{i, t})]; outcome != "" {
					fmt.Fprintf(&b, " %s %s;", target.ES, outcome)
				}
			}
		}
	}
	if c.extending {
		b.WriteString(" More items are being composed for this call.")
	}
	return b.String()
}

// Finish records each touched thought's status and the call in learner memory, and returns what the
// referee saw, for the debrief.
func (c *Class) Finish(session string, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeGap()
	type tally struct{ alone, found, total int }
	tallies := map[string]*tally{}
	var covered []string
	var report strings.Builder
	fmt.Fprintf(&report, "Referee report. Mode %s. Floor interventions: %d.", c.plan.Mode, c.stops)
	for i, item := range c.plan.Items {
		if !c.touched[item.Thought] {
			continue
		}
		t := tallies[item.Thought]
		if t == nil {
			t = &tally{}
			tallies[item.Thought] = t
			covered = append(covered, item.Thought)
		}
		fmt.Fprintf(&report, " [%s %s]", item.Kind, item.Thought)
		for k, target := range item.Targets {
			outcome := c.outcomes[c.key(&[2]int{i, k})]
			if outcome == "" || outcome == "skipped" {
				continue
			}
			t.total++
			if outcome == "alone" {
				t.alone++
			}
			if outcome == "alone" || outcome == "helped" {
				t.found++
			}
			fmt.Fprintf(&report, " %s: %s;", target.ES, outcome)
		}
	}
	statuses := map[string]string{}
	for id, t := range tallies {
		switch {
		case t.total > 0 && t.alone == t.total:
			statuses[id] = "found alone"
		case t.found > 0:
			statuses[id] = "found with help"
		default:
			statuses[id] = "introduced"
		}
	}
	c.record("closed", map[string]any{"statuses": statuses})
	c.learner.Update(func(l *Learner) {
		for id, status := range statuses {
			record := l.Thoughts[id]
			record.Status, record.At = status, now
			record.Seen++
			if status == "found alone" {
				record.Alone++
			}
			l.Thoughts[id] = record
		}
		l.Calls = append(l.Calls, LessonCall{At: c.started, Session: session, Minutes: now.Sub(c.started).Minutes(), Thoughts: covered})
	})
	return report.String()
}

// target is a plan target, a gap chunk (item -1), or the whole gap sentence (item -1, last index),
// where the part from the first gap onward also counts.
func (c *Class) target(at [2]int) Sentence {
	if at[0] >= 0 {
		return c.plan.Items[at[0]].Targets[at[1]]
	}
	if at[0] == -2 {
		return c.adhoc
	}
	if c.gap == nil {
		return Sentence{}
	}
	if at[1] < len(c.gap.Chunks) {
		chunk := c.gap.Chunks[at[1]]
		return Sentence{EN: chunk.English, ES: chunk.Spanish, Also: chunk.Also, Misses: chunk.Misses}
	}
	whole := Sentence{ES: c.gap.Sentence}
	lower := strings.ToLower(c.gap.Sentence)
	from := len(lower)
	for _, chunk := range c.gap.Chunks {
		if i := strings.Index(lower, strings.ToLower(chunk.Spanish)); i >= 0 {
			from = min(from, i)
		}
	}
	if from > 0 && from < len(lower) {
		whole.Also = []string{c.gap.Sentence[from:]}
	}
	return whole
}

func (c *Class) key(at *[2]int) string {
	if at == nil {
		return ""
	}
	if at[0] == -2 {
		return "adhoc"
	}
	if at[0] < 0 {
		return fmt.Sprintf("gap%d/%d", c.gapID, at[1]+1)
	}
	return fmt.Sprintf("%d:%s/%d", at[0]+1, c.plan.Items[at[0]].Thought, at[1]+1)
}

// record appends one floor event to the call's JSONL log: the pacing eval set.
func (c *Class) record(kind string, fields map[string]any) {
	if c.logPath == "" {
		return
	}
	event := map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "kind": kind}
	for k, v := range fields {
		event[k] = v
	}
	data, _ := json.Marshal(event)
	file, err := os.OpenFile(c.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	file.Write(append(data, '\n'))
}
