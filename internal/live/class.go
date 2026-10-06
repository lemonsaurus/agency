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
	holdFloor   = "Lemon is still building his answer and has only said part of it. Stop talking now. Stay silent and keep listening until he finishes it, says he doesn't know, or asks you something. No hints."
)

// Class runs one lesson call: it hands the live model the lesson one thought at a time, referees
// the floor after each target-sentence prompt, logs every floor event, and records the outcome.
type Class struct {
	mu      sync.Mutex
	lesson  Lesson
	learner *LearnerStore
	logPath string
	started time.Time
	outbox  chan update

	current  int  // thought being taught
	next     bool // the thought after current is already in the live model's context
	closing  bool // the closing step was handed over
	outcomes map[string]string
	misses   map[string]int
	touched  map[string]bool

	floor      string  // who holds the floor: rosa or lemon
	turn       string  // Rosa's words since Lemon last spoke
	armed      *[2]int // thought and sentence index of the prompt being worked on
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
}

type update struct{ kind, content string }

func newClass(lesson Lesson, learner *LearnerStore, logPath string, now time.Time) *Class {
	c := &Class{lesson: lesson, learner: learner, logPath: logPath, started: now, outbox: make(chan update, 64),
		outcomes: map[string]string{}, misses: map[string]int{}, touched: map[string]bool{}}
	for i, thought := range lesson.Thoughts {
		if learner.Status(thought.ID) != "found alone" {
			c.current = i
			break
		}
	}
	return c
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

// open hands over the mission, the lesson overview and the first thought.
func (c *Class) open() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.send("session.thinking.append", truncate("Today's lesson. "+c.lesson.Overview(c.learner.Status), updateLimit))
	c.handOver(c.current)
}

// handOver puts a thought's notes into the live model's context.
func (c *Class) handOver(i int) {
	thought := c.lesson.Thoughts[i]
	for _, note := range thought.Notes(fmt.Sprintf("%d of %d", i+1, len(c.lesson.Thoughts))) {
		c.send("session.thinking.append", note)
	}
	c.record("thought", map[string]any{"thought": thought.ID})
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
		c.record("attempt", map[string]any{"t": startMS, "sentence": c.key(c.armed), "think_ms": c.thinkMS})
	}
	c.held = false
	c.floor, c.turn = "lemon", ""
	c.attempt += text
	c.lemonEnd, c.lemonAt = endMS, time.Now()
	if c.armed == nil {
		return
	}
	sentence := c.sentence(*c.armed)
	verdict := judge(c.attempt, sentence)
	if verdict != c.verdict {
		c.verdict = verdict
		c.record("verdict", map[string]any{"t": endMS, "sentence": c.key(c.armed), "verdict": verdict, "said": truncate(c.attempt, 300)})
	}
	if verdict == verdictRight {
		outcome := "alone"
		if c.misses[c.key(c.armed)] > 0 {
			outcome = "helped"
		}
		c.resolve(*c.armed, outcome)
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
	if c.armed != nil && c.misses[c.key(c.armed)] > 0 && says(turn, c.sentence(*c.armed).ES) {
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
	c.record("intervene", map[string]any{"sentence": c.key(c.armed), "said": truncate(c.attempt, 300), "rosa": truncate(c.turn, 300)})
	c.send("session.instructions.append", holdFloor)
}

// cue arms the open sentence whose English cue ends Rosa's turn, and reports whether one did. A
// one-word cue must be the turn's last word; a longer one may trail two words, as in "..., che?".
func (c *Class) cue(turn []string) bool {
	best, at := (*[2]int)(nil), -1
	for t := c.current; t <= c.current+1 && t < len(c.lesson.Thoughts); t++ {
		if t > c.current && !c.next {
			break
		}
		for i, sentence := range c.lesson.Thoughts[t].Sentences {
			if c.outcomes[c.key(&[2]int{t, i})] != "" {
				continue
			}
			slack := 2
			if len(words(sentence.EN)) == 1 {
				slack = 0
			}
			if end := cued(turn, sentence.EN); end >= 0 && end >= len(turn)-1-slack && end >= at {
				best, at = &[2]int{t, i}, end
			}
		}
	}
	if best == nil {
		return false
	}
	if c.armed != nil && *c.armed == *best {
		return true
	}
	if best[0] > c.current {
		c.advance(best[0])
	}
	c.armed, c.verdict = best, ""
	c.touched[c.lesson.Thoughts[best[0]].ID] = true
	c.record("prompt", map[string]any{"sentence": c.key(best), "en": c.sentence(*best).EN})
	if best[1] == len(c.lesson.Thoughts[best[0]].Sentences)-1 {
		c.lookAhead()
	}
	return true
}

// lookAhead hands over the next thought, or the closing step, while the last prompt is answered.
func (c *Class) lookAhead() {
	if c.next || c.closing {
		return
	}
	if c.current+1 < len(c.lesson.Thoughts) {
		c.next = true
		c.handOver(c.current + 1)
		return
	}
	c.closing = true
	c.send("session.thinking.append", truncate("Last step, after this prompt: "+c.lesson.Close, updateLimit))
	c.record("close", nil)
}

// advance moves the lesson to thought i; prompts skipped on the way are marked skipped.
func (c *Class) advance(i int) {
	for t := c.current; t < i; t++ {
		for s := range c.lesson.Thoughts[t].Sentences {
			if key := c.key(&[2]int{t, s}); c.outcomes[key] == "" && c.touched[c.lesson.Thoughts[t].ID] {
				c.outcomes[key] = "skipped"
			}
		}
	}
	c.current, c.next = i, false
}

func (c *Class) resolve(at [2]int, outcome string) {
	key := c.key(&at)
	c.outcomes[key] = outcome
	for i := 0; i < at[1]; i++ {
		if earlier := c.key(&[2]int{at[0], i}); c.outcomes[earlier] == "" {
			c.outcomes[earlier] = "skipped"
		}
	}
	c.armed, c.verdict, c.held = nil, "", false
	sentence := c.sentence(at)
	c.record("resolved", map[string]any{"sentence": key, "outcome": outcome, "think_ms": c.thinkMS})
	think := c.thinkMS
	c.learner.Update(func(l *Learner) {
		l.Used = append(l.Used, sentence.ES)
		l.Pacing = append(l.Pacing, Pace{At: time.Now(), Sentence: sentence.ES, ThinkMS: think, Outcome: outcome})
	})
}

// Position is where the lesson stands, for Rosa's backend.
func (c *Class) Position() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	thought := c.lesson.Thoughts[c.current]
	var b strings.Builder
	fmt.Fprintf(&b, "Lesson %s: teaching thought %d of %d, %s [%s].", c.lesson.ID, c.current+1, len(c.lesson.Thoughts), thought.Title, thought.ID)
	if c.armed != nil {
		fmt.Fprintf(&b, " Open prompt: %q → %s.", c.sentence(*c.armed).EN, c.sentence(*c.armed).ES)
	}
	if len(c.outcomes) > 0 {
		b.WriteString(" Outcomes so far:")
		for t, th := range c.lesson.Thoughts {
			for i, sentence := range th.Sentences {
				if outcome := c.outcomes[c.key(&[2]int{t, i})]; outcome != "" {
					fmt.Fprintf(&b, " %s %s;", sentence.ES, outcome)
				}
			}
		}
	}
	if c.closing {
		b.WriteString(" The closing step has been handed over.")
	}
	return b.String()
}

// Move jumps the lesson to a thought by id and hands it over.
func (c *Class) Move(id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, thought := range c.lesson.Thoughts {
		if thought.ID == id {
			c.advance(i)
			c.armed, c.closing = nil, false
			c.handOver(i)
			return "Thought " + thought.Title + " is now in the call's notes; Rosa teaches it next.", nil
		}
	}
	return "", fmt.Errorf("no thought %q in lesson %s", id, c.lesson.ID)
}

// Finish records each touched thought's status and the call in learner memory, and returns what the
// referee saw, for the debrief.
func (c *Class) Finish(session string, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var covered []string
	var report strings.Builder
	fmt.Fprintf(&report, "Referee report for lesson %s. Floor interventions: %d.", c.lesson.ID, c.stops)
	statuses := map[string]string{}
	for t, thought := range c.lesson.Thoughts {
		if !c.touched[thought.ID] {
			continue
		}
		covered = append(covered, thought.ID)
		alone, found, total := 0, 0, 0
		for i, sentence := range thought.Sentences {
			outcome := c.outcomes[c.key(&[2]int{t, i})]
			if outcome == "" || outcome == "skipped" {
				continue
			}
			total++
			if outcome == "alone" {
				alone++
			}
			if outcome == "alone" || outcome == "helped" {
				found++
			}
			fmt.Fprintf(&report, " %s: %s;", sentence.ES, outcome)
		}
		switch {
		case total > 0 && alone == total:
			statuses[thought.ID] = "found alone"
		case found > 0:
			statuses[thought.ID] = "found with help"
		default:
			statuses[thought.ID] = "introduced"
		}
	}
	c.record("closed", map[string]any{"statuses": statuses})
	c.learner.Update(func(l *Learner) {
		for id, status := range statuses {
			l.Thoughts[id] = ThoughtStatus{Status: status, Lesson: c.lesson.ID, At: now}
		}
		l.Calls = append(l.Calls, LessonCall{At: c.started, Session: session, Minutes: now.Sub(c.started).Minutes(), Thoughts: covered})
	})
	return report.String()
}

func (c *Class) sentence(at [2]int) Sentence {
	return c.lesson.Thoughts[at[0]].Sentences[at[1]]
}

func (c *Class) key(at *[2]int) string {
	if at == nil {
		return ""
	}
	return fmt.Sprintf("%s/%d", c.lesson.Thoughts[at[0]].ID, at[1]+1)
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
