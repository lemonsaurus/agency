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
	settleDelay = 300 * time.Millisecond
	// A code-switched turn is sent for gap help after this much silence.
	switchPause = 1500 * time.Millisecond
	// A target's English cue arms it when it ends within this many words of the end of Rosa's turn.
	cueWindow    = 10
	holdFloor    = "Lemon is still building his answer and has only said part of it. Stop talking now. Stay silent and keep listening until he finishes it, says he doesn't know, or asks you something. No hints."
	releaseFloor = "Lemon has finished his answer. The floor is yours again: respond to what he said now."
	answerHim    = "Lemon is calling you or checking you are there. Answer in one short phrase, no greeting, and carry on where you were."
	takeAnswer   = "Lemon answered while you were still talking. Stop, and respond to his answer now."
	moveOn       = "Nothing is pending. Go straight on to the next step now."
	waitForHim   = "You just asked him something and he is formulating his answer. Stop talking and wait in silence; he needs at least eight seconds."
	talkToHim    = "Lemon is talking to you about the lesson, not answering. Answer him, let the open sentence go, and adapt to what he said."
	repeated     = "You already said that this call. Don't repeat yourself: say or ask something new."
	sayAgain     = "Lemon didn't catch the phrase and is asking in English. That is not an answer. Repeat the English phrase slowly and clearly, or confirm it, then wait for his Spanish."
	// Dead air after Rosa's turn, with nothing asked of Lemon, before the box nudges her on.
	idleLimit = 4 * time.Second
	// A target he keeps not getting, with no new cue, is let go after this many attempts.
	armedLimit = 3
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
	id      string // the thread's first session; resumed calls keep it
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
	recorded  map[string]string // statuses already written to learner memory in this thread
	misses    map[string]int
	hinted    map[string]bool // Rosa gave part of the answer, or he lacked a word
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
	holding    bool // Rosa was told to wait; released when his attempt completes or he goes quiet
	holdTimer  *time.Timer
	overlap    bool // he started this attempt while Rosa was still talking
	tookOver   bool
	tries      int      // attempts since the armed target was last cued
	praised    string   // a target just resolved alone, until Rosa's reply shows whether she agreed
	called     bool     // he called her by name or checked she is there in this attempt
	previous   []string // his words in the attempt before this one, which her echo doesn't count as a hint
	prompts    []string // every target asked this call, so nothing is asked twice
	harder     time.Time
	listen     int             // plan item being decoded by ear, or -1
	understood map[int]bool    // chunks of that passage he has decoded
	spoken     map[string]bool // every sentence Rosa has said this call, normalised
	logged     map[int]bool    // items whose topics went into the topic log
	turnDone   int             // sentences of her current turn already checked
	warned     bool            // she was told about a repeat in this turn
	askedAt    time.Time       // when her last turn ended on a question to him
	waitStep   int             // how far up the wait ladder the silence has gone
	waitTimer  *time.Timer
	waited     bool // she was told to wait in this silence
	// onHarder asks the planner to jump ahead when he says it's too easy.
	onHarder  func()
	idleTimer *time.Timer
	rosaEnd   int64
	lemonEnd  int64
	lemonAt   time.Time
	thinkMS   int64
	stops     int
	switchAt  *time.Timer
}

// waitLadder is what Rosa may do as the silence after her question grows: each step comes after its
// delay of quiet since she last spoke, and Lemon speaking starts the ladder over.
var waitLadder = []struct {
	after       time.Duration
	instruction string
}{
	{8 * time.Second, "He has thought for eight seconds. One short, warm encouragement, a few words, then wait again."},
	{10 * time.Second, "Still thinking. Offer to split it: ask for just the first piece, then wait."},
	{12 * time.Second, "Give him the first word or piece, then wait for the rest."},
	{15 * time.Second, "Give him the answer, have him say it, and move on."},
}

// holdLimit is the most silence from Lemon a hold on Rosa outlasts.
var holdLimit = 8 * time.Second

type update struct{ kind, content string }

func newClass(id string, graph *Graph, learner *LearnerStore, logPath string, now time.Time) *Class {
	return &Class{id: id, graph: graph, learner: learner, logPath: logPath, started: now, outbox: make(chan update, 64),
		handed: map[int]bool{}, recorded: map[string]string{}, listen: -1, understood: map[int]bool{}, spoken: map[string]bool{}, logged: map[int]bool{}, outcomes: map[string]string{}, misses: map[string]int{}, hinted: map[string]bool{}, touched: map[string]bool{}}
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

// thread is a class's state between calls, saved so a resumed call survives a daemon restart.
type thread struct {
	ID       string            `json:"id"`
	Started  time.Time         `json:"started"`
	Ended    time.Time         `json:"ended"`
	Plan     Plan              `json:"plan"`
	Planned  bool              `json:"planned"`
	Current  int               `json:"current"`
	Handed   map[int]bool      `json:"handed"`
	Outcomes map[string]string `json:"outcomes"`
	Recorded map[string]string `json:"recorded"`
	Misses   map[string]int    `json:"misses"`
	Hinted   map[string]bool   `json:"hinted"`
	Touched  map[string]bool   `json:"touched"`
}

func (c *Class) snapshot(ended time.Time) thread {
	c.mu.Lock()
	defer c.mu.Unlock()
	return thread{c.id, c.started, ended, c.plan, c.planned, c.current, c.handed, c.outcomes, c.recorded, c.misses, c.hinted, c.touched}
}

func restore(t thread, graph *Graph, learner *LearnerStore) *Class {
	c := newClass(t.ID, graph, learner, "", t.Started)
	c.plan, c.planned, c.current = t.Plan, t.Planned, t.Current
	if t.Handed != nil {
		c.handed = t.Handed
	}
	if t.Outcomes != nil {
		c.outcomes = t.Outcomes
	}
	if t.Recorded != nil {
		c.recorded = t.Recorded
	}
	if t.Misses != nil {
		c.misses = t.Misses
	}
	if t.Hinted != nil {
		c.hinted = t.Hinted
	}
	if t.Touched != nil {
		c.touched = t.Touched
	}
	return c
}

// Resume carries the thread into a new call after a drop: fresh floor state, and the plan overview
// and the items in play handed over again, since the new session has never seen them.
func (c *Class) Resume(logPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logPath = logPath
	c.floor, c.turn, c.attempt, c.verdict = "", "", "", ""
	c.rosaEnd, c.lemonEnd = 0, 0
	c.armed, c.held, c.holding, c.overlap, c.fresh = nil, false, false, false, false
	if c.holdTimer != nil {
		c.holdTimer.Stop()
	}
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	c.record("resumed", map[string]any{"thread": c.id})
	if !c.planned {
		return
	}
	for _, note := range c.plan.Overview(c.graph) {
		c.send("session.thinking.append", note)
	}
	for i := c.current; i < len(c.plan.Items); i++ {
		if c.handed[i] || i == c.current {
			c.handOver(i)
		}
	}
}

// SetPlan hands over the plan's overview and its first item.
func (c *Class) SetPlan(plan Plan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plan, c.planned = plan, true
	for _, note := range plan.Overview(c.graph) {
		c.send("session.thinking.append", note)
	}
	c.record("plan", map[string]any{"mode": plan.Mode, "why": plan.Why, "items": plan.Items, "teasers": plan.Teasers})
	if cautions := c.learner.Cautions(); len(cautions) > 0 {
		c.send("session.thinking.append", truncate("Facts to get right from now on. Use them silently: no apology, no talk about earlier calls; at most one short clause, once, if it comes up. "+strings.Join(cautions, " "), updateLimit))
	}
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
	if len(c.prompts) > 0 {
		c.send("session.thinking.append", truncate("Already asked this call, never ask these again: "+strings.Join(c.prompts, "; ")+".", updateLimit))
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
	case "error":
		// A model error mid-call may be moderation cutting her off; logged so it can be reviewed.
		c.record("error", map[string]any{"error": truncate(text, 300), "rosa": truncate(c.turn, 300)})
	}
}

func (c *Class) heard(text string, startMS, endMS int64) {
	text = speech(text)
	if strings.TrimSpace(text) == "" {
		return
	}
	if c.floor != "lemon" && !c.held {
		c.overlap = c.floor == "rosa" && startMS < c.rosaEnd+300
		c.fresh = c.cue(c.turn)
		if c.fresh {
			c.tries = 0
		} else if c.armed != nil {
			// An uncued attempt keeps a target only while a correction is under way.
			if key := c.key(c.armed); c.armed[0] < 0 || c.misses[key] == 0 && !c.hinted[key] || c.tries >= armedLimit {
				c.armed = nil
			}
		}
		c.tries++
		c.previous = words(c.attempt)
		c.attemptID++
		c.attempt, c.verdict, c.intervened, c.tookOver, c.called = "", "", false, false, false
		c.thinkMS = 0
		if c.rosaEnd > 0 {
			c.thinkMS = startMS - c.rosaEnd
		}
		c.record("attempt", map[string]any{"t": startMS, "target": c.key(c.armed), "think_ms": c.thinkMS})
	}
	c.held = false
	c.floor, c.turn, c.turnDone, c.warned = "lemon", "", 0, false
	c.waitStep, c.waited, c.askedAt = 0, false, time.Time{}
	if c.waitTimer != nil {
		c.waitTimer.Stop()
	}
	c.attempt += text
	c.lemonEnd, c.lemonAt = endMS, time.Now()
	if c.listen >= 0 && c.listen == c.current {
		c.decode()
		return
	}
	if switched(c.attempt) && c.onSwitch != nil {
		if c.switchAt != nil {
			c.switchAt.Stop()
		}
		id, said := c.attemptID, c.attempt
		c.switchAt = time.AfterFunc(switchPause, func() { c.switchCheck(id, said) })
	}
	if c.armed != nil {
		verdict := judge(c.attempt, c.target(*c.armed))
		if verdict != verdictRight && addressed(c.attempt) {
			verdict = verdictAddressed
		}
		if verdict != c.verdict {
			c.verdict = verdict
			c.record("verdict", map[string]any{"t": endMS, "target": c.key(c.armed), "verdict": verdict, "said": truncate(c.attempt, 300)})
		}
		if verdict == verdictMeta {
			// He is talking about the lesson, not answering: she answers him and the target goes.
			c.armed = nil
			if c.holding {
				c.release(talkToHim)
			}
			return
		}
		// The first hesitation after a prompt holds her before she can start anything else.
		if c.fresh && verdict == verdictPartial && !c.holding && !c.intervened {
			c.intervened, c.holding = true, true
			c.stops++
			c.holdUntilQuiet()
			c.record("hold", map[string]any{"target": c.key(c.armed), "said": truncate(c.attempt, 300)})
			c.send("session.instructions.append", holdFloor)
		}
	}
	if easier(c.attempt) && c.onHarder != nil && time.Since(c.harder) > 2*time.Minute {
		c.harder = time.Now()
		c.record("harder", map[string]any{"said": truncate(c.attempt, 300)})
		c.send("session.thinking.append", "He wants harder material. New items from further on are being prepared; meanwhile go up a level yourself with longer, fresher sentences, and don't repeat anything already asked.")
		go c.onHarder()
	}
	if c.verdict == verdictClarify {
		id, said := c.attemptID, c.attempt
		time.AfterFunc(settleDelay, func() { c.clarify(id, said) })
	}
	if addressed(c.attempt) && !c.holding && !c.called {
		c.called = true
		c.record("addressed", map[string]any{"said": truncate(c.attempt, 300)})
		c.send("session.instructions.append", c.where(answerHim))
	}
	if c.overlap && c.fresh && !c.tookOver && c.armed != nil && c.verdict != verdictPartial && c.verdict != verdictClarify {
		c.tookOver = true
		c.record("take", map[string]any{"target": c.key(c.armed), "said": truncate(c.attempt, 300)})
		c.send("session.instructions.append", takeAnswer)
	}
	if c.holding {
		switch {
		case addressed(c.attempt):
			c.release(c.where(answerHim))
		case c.armed == nil || c.verdict != verdictPartial:
			c.release(c.where(releaseFloor))
		default:
			c.holdUntilQuiet()
		}
	}
	if c.armed == nil {
		return
	}
	verdict := c.verdict
	if verdict == verdictRight {
		outcome := "alone"
		if key := c.key(c.armed); c.misses[key] > 0 || c.hinted[key] {
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
// A pass for an earlier, shorter cut of the turn that finishes late is dropped.
func (c *Class) SetGap(gap Gap) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(gap.Chunks) == 0 || gap.Said != c.asked {
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
	// Her question, then a pause, then her voice again: she is filling his think time.
	if c.floor == "rosa" && !c.askedAt.IsZero() && startMS-c.rosaEnd >= 1000 && time.Since(c.askedAt) < waitLadder[0].after && !c.waited && c.waitStep == 0 {
		c.waited = true
		c.record("wait", map[string]any{"rosa": truncate(text, 200)})
		c.send("session.instructions.append", waitForHim)
	}
	c.turn += text
	c.rosaEnd = endMS
	turn := words(c.turn)
	c.repeats()
	c.enter(turn)
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	said := c.turn
	c.idleTimer = time.AfterFunc(idleLimit, func() { c.idle(said) })
	if c.waitTimer != nil {
		c.waitTimer.Stop()
	}
	if c.asking(said) || c.waited {
		if c.askedAt.IsZero() {
			c.askedAt = time.Now()
		}
		if c.waitStep < len(waitLadder) {
			step := c.waitStep
			c.waitTimer = time.AfterFunc(waitLadder[step].after, func() { c.climb(said, step) })
		}
	} else {
		c.askedAt = time.Time{}
	}
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
			case verdictGap:
				c.hinted[c.key(c.armed)] = true
			}
		}
	}
	if c.praised != "" && len(turn) >= 3 {
		if corrects(turn) {
			c.outcomes[c.praised] = "helped"
			c.record("downgrade", map[string]any{"target": c.praised, "rosa": truncate(c.turn, 300)})
		}
		c.praised = ""
	}
	if c.armed == nil || c.floor != "rosa" {
		return
	}
	target := c.target(*c.armed)
	if hints(turn, target, c.previous) {
		c.hinted[c.key(c.armed)] = true
	}
	if c.verdict != verdictClarify && c.misses[c.key(c.armed)] > 0 && says(turn, target.ES) {
		c.resolve(*c.armed, "shown")
	}
}

// clarify tells Rosa to repeat the phrase when Lemon asked about it in English instead of answering.
func (c *Class) clarify(id int, said string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id != c.attemptID || said != c.attempt || c.verdict != verdictClarify {
		return
	}
	c.record("clarify", map[string]any{"target": c.key(c.armed), "said": truncate(said, 300)})
	c.send("session.instructions.append", sayAgain)
}

// finished is whether Rosa's turn ended on a full stop: not a question, and not a lead-in like "and
// then" or "so" that she is about to continue.
func finished(turn string) bool {
	trimmed := strings.TrimSpace(turn)
	if !strings.HasSuffix(trimmed, ".") && !strings.HasSuffix(trimmed, "!") {
		return false
	}
	last := words(trimmed)
	if len(last) == 0 {
		return false
	}
	switch last[len(last)-1] {
	case "and", "so", "then", "but", "because", "which", "is", "like", "or", "now", "first", "next":
		return false
	}
	return true
}

// corrects is whether Rosa's reply opens by correcting him rather than agreeing.
func corrects(turn []string) bool {
	if turn[0] == "no" && (len(turn) == 1 || !spanish[turn[1]] && !spanishEnding(turn[1])) {
		return true
	}
	opening := " " + strings.Join(turn[:min(len(turn), 6)], " ") + " "
	for _, phrase := range []string{" not quite ", " almost ", " close ", " careful ", " try again ", " nope ", " do not give up ", " hmm "} {
		if strings.Contains(opening, phrase) {
			return true
		}
	}
	return false
}

// repeats checks each sentence Rosa finishes against everything she has said this call, and tells her
// at once when she repeats one. Short stock lines and a re-ask of the target he is still working on
// are fine.
func (c *Class) repeats() {
	sentences := strings.FieldsFunc(c.turn, func(r rune) bool { return r == '.' || r == '?' || r == '!' })
	if !strings.ContainsAny(c.turn[max(len(c.turn)-1, 0):], ".?!") {
		sentences = sentences[:max(len(sentences)-1, 0)]
	}
	open := ""
	if c.armed != nil && c.outcomes[c.key(c.armed)] == "" {
		open = strings.Join(words(c.target(*c.armed).EN), " ")
	}
	for ; c.turnDone < len(sentences); c.turnDone++ {
		tokens := words(sentences[c.turnDone])
		if len(tokens) < 5 {
			continue
		}
		key := strings.Join(tokens, " ")
		if c.spoken[key] && !c.warned && (open == "" || !strings.Contains(key, open)) {
			c.warned = true
			c.record("repeat", map[string]any{"rosa": truncate(sentences[c.turnDone], 300)})
			c.send("session.instructions.append", repeated)
		}
		c.spoken[key] = true
	}
}

// enter moves the call into a life or listen item once Rosa starts it: she asks its question, or tells
// its story. The item after it is handed over at once so she always has the next step.
func (c *Class) enter(turn []string) {
	for i := c.current + 1; i < len(c.plan.Items); i++ {
		item := c.plan.Items[i]
		if !c.handed[i] {
			continue
		}
		started := false
		switch item.Type {
		case "life":
			started = overlaps(turn, item.Question)
		case "listen":
			started = len(item.Passage.Chunks) > 0 && says(turn, item.Passage.Chunks[0].ES)
		}
		if !started {
			continue
		}
		c.advance(i)
		c.touched[item.Thought] = true
		if item.Type == "listen" {
			c.listen, c.understood = i, map[int]bool{}
		}
		c.record("enter", map[string]any{"item": i, "type": item.Type})
		c.lookAhead()
		return
	}
}

// decode marks the chunks of the passage whose meaning his English decoding carries, and moves on once
// he has them all.
func (c *Class) decode() {
	item := c.plan.Items[c.listen]
	for i, piece := range item.Passage.Chunks {
		if !c.understood[i] && overlaps(words(c.attempt), piece.EN) {
			c.understood[i] = true
			c.record("understood", map[string]any{"chunk": piece.ES, "said": truncate(c.attempt, 200)})
			es := piece.ES
			c.learner.Update(func(l *Learner) { l.hear(es, time.Now()) })
		}
	}
	if len(c.understood) == len(item.Passage.Chunks) {
		c.record("decoded", map[string]any{"item": c.listen})
		c.listen = -1
		c.lookAhead()
	}
}

// overlaps is whether turn carries at least half of the content words of text.
func overlaps(turn []string, text string) bool {
	have := map[string]bool{}
	for _, word := range turn {
		have[word] = true
	}
	total, hit := 0, 0
	for _, word := range words(text) {
		if len(word) < 3 || idleWords[word] {
			continue
		}
		total++
		if have[word] {
			hit++
		}
	}
	return total > 0 && hit*2 >= total
}

// asking is whether her turn ends by asking him something: a question, a translation prompt, an
// instruction to say or build something, or a planned target's cue.
func (c *Class) asking(turn string) bool {
	if strings.HasSuffix(strings.TrimSpace(turn), "?") || instructs(turn) {
		return true
	}
	if _, ok := improvised(turn); ok {
		return true
	}
	tokens := words(turn)
	for item := c.current; item < len(c.plan.Items); item++ {
		if !c.handed[item] {
			continue
		}
		for _, target := range c.plan.Items[item].Targets {
			if end := cued(tokens, target.EN); end >= 0 && end >= len(tokens)-1-cueWindow {
				return true
			}
		}
	}
	return false
}

// climb takes the next step up the wait ladder if the silence after her question has lasted.
func (c *Class) climb(said string, step int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.turn != said || c.floor != "rosa" || c.waitStep != step || c.holding {
		return
	}
	c.waitStep++
	c.record("encourage", map[string]any{"step": step + 1})
	c.send("session.instructions.append", waitLadder[step].instruction)
}

// idle nudges Rosa on when she has stopped after a turn that asked nothing of Lemon.
func (c *Class) idle(said string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.turn != said || c.floor != "rosa" || c.armed != nil || c.holding || !finished(said) || c.asking(said) || c.waited {
		return
	}
	c.record("nudge", map[string]any{"rosa": truncate(said, 300)})
	c.send("session.instructions.append", moveOn)
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
	c.intervened, c.holding = true, true
	c.holdUntilQuiet()
	c.stops++
	c.record("intervene", map[string]any{"target": c.key(c.armed), "said": truncate(c.attempt, 300), "rosa": truncate(c.turn, 300)})
	c.send("session.instructions.append", holdFloor)
}

// holdUntilQuiet releases the hold once Lemon has said nothing new for holdLimit.
func (c *Class) holdUntilQuiet() {
	if c.holdTimer != nil {
		c.holdTimer.Stop()
	}
	said := c.attempt
	c.holdTimer = time.AfterFunc(holdLimit, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.holding && c.attempt == said {
			c.release(c.where(releaseFloor))
		}
	})
}

// where adds what Rosa was doing to an instruction, so she carries on instead of starting over.
func (c *Class) where(instruction string) string {
	if c.armed == nil {
		return instruction
	}
	if en := c.target(*c.armed).EN; en != "" {
		return instruction + fmt.Sprintf(" You were asking him how to say %q; carry on with that.", en)
	}
	return instruction
}

// release lifts a hold on Rosa.
func (c *Class) release(instruction string) {
	c.holding, c.held = false, false
	if c.holdTimer != nil {
		c.holdTimer.Stop()
	}
	c.record("release", map[string]any{"said": truncate(c.attempt, 300), "instruction": instruction})
	c.send("session.instructions.append", instruction)
}

// cue arms the open target whose English cue Rosa said last, within the last words of her turn,
// and reports whether one did: gap chunks first, then the plan items that reached the call, then the
// whole gap sentence once Rosa has worked a chunk or given one, then a prompt she improvised.
func (c *Class) cue(raw string) bool {
	turn := words(raw)
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
		prompt, ok := improvised(raw)
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
	if hints(turn, c.target(*best), c.previous) {
		c.hinted[c.key(best)] = true
	}
	c.record("prompt", map[string]any{"target": c.key(best), "en": c.target(*best).EN, "es": c.target(*best).ES})
	if prompt := c.target(*best); prompt.EN != "" {
		c.prompts = append(c.prompts, prompt.EN)
	}
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
	if c.listen >= 0 && i > c.listen {
		c.listen = -1
	}
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
	if outcome == "alone" {
		c.praised = key
		c.fastLane(at[0])
	}
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

// fastLane lets go of the rest of an item once he has three clean, cold answers on it and nothing
// wrong: he owns it, so Rosa moves straight on.
func (c *Class) fastLane(item int) {
	if item < 0 {
		return
	}
	clean, open := 0, 0
	for t := range c.plan.Items[item].Targets {
		key := c.key(&[2]int{item, t})
		switch c.outcomes[key] {
		case "alone":
			clean++
		case "":
			open++
		default:
			return
		}
		if c.misses[key] > 0 {
			return
		}
	}
	if clean < 3 || open == 0 {
		return
	}
	for t := range c.plan.Items[item].Targets {
		if key := c.key(&[2]int{item, t}); c.outcomes[key] == "" {
			c.outcomes[key] = "owned"
		}
	}
	c.record("owned", map[string]any{"item": item, "thought": c.plan.Items[item].Thought})
	c.send("session.thinking.append", "He owns this one: three clean answers. Skip its remaining targets and go straight to the next item.")
	c.lookAhead()
}

// Touches is how many items in this call worked on a thought.
func (c *Class) Touches(thought string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for i, item := range c.plan.Items {
		if item.Thought == thought && (c.handed[i] || c.touched[thought]) {
			n++
		}
	}
	return n
}

// Asked is every target asked so far this call.
func (c *Class) Asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.prompts...)
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
// referee saw, for the debrief. It runs at the end of every call in a thread and only writes what
// changed; a thought touched without any resolved target never loses the status it had.
func (c *Class) Finish(now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeGap()
	report, statuses, covered := c.tally()
	c.record("closed", map[string]any{"statuses": statuses})
	var topics []string
	for i, item := range c.plan.Items {
		if c.handed[i] && !c.logged[i] {
			c.logged[i] = true
			topics = append(topics, item.Topics...)
		}
	}
	recorded := c.recorded
	c.learner.Update(func(l *Learner) {
		for id, status := range statuses {
			if recorded[id] == status {
				continue
			}
			record := l.Thoughts[id]
			if recorded[id] == "" {
				record.Seen++
			}
			if status == "found alone" {
				record.Alone++
			} else if recorded[id] == "found alone" {
				record.Alone--
			}
			record.Status, record.At = status, now
			l.Thoughts[id] = record
			recorded[id] = status
		}
		for _, topic := range topics {
			l.Topics = append(l.Topics, TopicUse{Topic: topic, At: now})
		}
		call := LessonCall{At: c.started, Session: c.id, Minutes: now.Sub(c.started).Minutes(), Thoughts: covered}
		for i := range l.Calls {
			if l.Calls[i].Session == c.id {
				call.Summary = l.Calls[i].Summary
				l.Calls[i] = call
				return
			}
		}
		l.Calls = append(l.Calls, call)
	})
	return report
}

// Report is what the referee saw in the thread so far, for the debrief.
func (c *Class) Report() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	report, _, _ := c.tally()
	return report
}

// tally sums the outcomes per thought: the referee report, each touched thought's status, and the
// thoughts covered.
func (c *Class) tally() (string, map[string]string, []string) {
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
			if outcome == "alone" || outcome == "owned" {
				t.alone++
			}
			if outcome == "alone" || outcome == "owned" || outcome == "helped" {
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
		case t.total > 0 || c.learner.Status(id) == "not introduced":
			statuses[id] = "introduced"
		}
	}
	return report.String(), statuses, covered
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
