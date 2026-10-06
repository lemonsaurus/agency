package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// A call shorter than this leaves the waiting plan as it is.
	debriefAfter = 3 * time.Minute
	gapModel     = "gpt-6-luna"
)

// Rosa is the Spanish tutor voice: her own persona, prompts, voice, transcript memory, learner
// memory, thought graph and tools. She never loads Carla's persona and never sees the box's panes.
type Rosa struct {
	prompts string // ~/.agents/voice/rosa
	dir     string // ~/.agents/run/agency/rosa
	memory  *Memory
	learner *LearnerStore

	mu      sync.Mutex
	graph   *Graph
	class   *Class
	turnOff func() string
}

// RosaSchema is the function list Rosa's backend sees.
var RosaSchema = json.RawMessage(`[
{"type":"function","name":"plan","description":"Where this call stands: mode, the plan item being taught, the open target, and each target's outcome so far.","parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}},
{"type":"function","name":"find","description":"Search the thought graph by words in a thought's id, title, kind or explanations. Returns ids, titles, kinds and prerequisites.","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}},
{"type":"function","name":"thought","description":"One thought in full, with Lemon's status for it: explanations, reframes, Norwegian import, cross-language links, example targets and known near misses.","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}},
{"type":"function","name":"author","description":"Add a new thought to the graph, with source rosa: a vocabulary family, lunfardo, an idiom, a mood-tense feeling, dialect, or a structure his Spanish needs next. needs must be existing ids. links are true etymology or shared structure with Norwegian, English, Swedish, German, Greek, French or Latin; leave out any you are not sure of.","parameters":{"type":"object","properties":{"id":{"type":"string","description":"kebab-case, prefixed r-"},"title":{"type":"string"},"kind":{"type":"string","enum":["structure","sound","convert","vocab","slang","idiom","mood","dialect","habit"]},"needs":{"type":"array","items":{"type":"string"}},"teach":{"type":"array","items":{"type":"string"}},"reframes":{"type":"array","items":{"type":"string"}},"import":{"type":"string"},"links":{"type":"array","items":{"type":"string"}},"examples":{"type":"array","items":{"type":"object","properties":{"en":{"type":"string"},"es":{"type":"string"}},"required":["en","es"],"additionalProperties":false}},"misses":{"type":"array","items":{"type":"object","properties":{"said":{"type":"string"},"cause":{"type":"string"},"ask":{"type":"string"}},"required":["said","cause","ask"],"additionalProperties":false}},"aside":{"type":"string"}},"required":["id","title","kind","needs","teach","reframes","import","links","examples","misses","aside"],"additionalProperties":false}},
{"type":"function","name":"teach","description":"Teach a thought next in this call, with fresh targets you write from his life. Its notes reach the call at once.","parameters":{"type":"object","properties":{"id":{"type":"string"},"why":{"type":"string"},"targets":{"type":"array","items":{"type":"object","properties":{"en":{"type":"string"},"es":{"type":"string"},"also":{"type":"array","items":{"type":"string"}},"note":{"type":"string"}},"required":["en","es","also","note"],"additionalProperties":false}}},"required":["id","why","targets"],"additionalProperties":false}},
{"type":"function","name":"status","description":"Set Lemon's status for a thought the call covered when the referee report missed it, judged from the transcript.","parameters":{"type":"object","properties":{"id":{"type":"string"},"status":{"type":"string","enum":["introduced","found with help","found alone"]}},"required":["id","status"],"additionalProperties":false}},
{"type":"function","name":"talk","description":"Switch the rest of this call to conversation when Lemon wants to just talk.","parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}},
{"type":"function","name":"learner","description":"Everything learner memory holds about Lemon: profile facts, thought statuses, errors by cause, habits, the word dictionary, gaps, links he reacted to, wording log, pacing and past calls.","parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}},
{"type":"function","name":"remember","description":"Add to learner memory. profile: a durable fact about Lemon's life to build sentences from. error: a wrong answer filed under its diagnosed cause (text is the cause; give thought, said and expected). habit: a learning habit seen, such as reciting tables, guessing, or inventing mnemonics. link: a cross-language link and how he reacted to it. Never store anything Lemon asks you not to.","parameters":{"type":"object","properties":{"kind":{"type":"string","enum":["profile","error","habit","link"]},"text":{"type":"string"},"thought":{"type":"string"},"said":{"type":"string"},"expected":{"type":"string"}},"required":["kind","text","thought","said","expected"],"additionalProperties":false}},
{"type":"function","name":"words","description":"Add Spanish words Lemon was exposed to into his dictionary, with how he got them and the sentence they came in, and what he has memorised: a noun's gender, a set of conjugation endings, an irregular form. Links only when true.","parameters":{"type":"object","properties":{"entries":{"type":"array","items":{"type":"object","properties":{"word":{"type":"string"},"kind":{"type":"string","enum":["word","gender","endings","form"],"description":"word, or what he has memorised: a noun's gender, a set of conjugation endings, an irregular form."},"gender":{"type":"string","enum":["","el","la"]},"how":{"type":"string","enum":["told","found with help","found alone"]},"sentence":{"type":"string"},"links":{"type":"array","items":{"type":"string"}}},"required":["word","kind","gender","how","sentence","links"],"additionalProperties":false}}},"required":["entries"],"additionalProperties":false}},
{"type":"function","name":"conversation","description":"End the call when Lemon is done or asks to stop. The phone hangs up after your next sentence, so say a short goodbye.","parameters":{"type":"object","properties":{"state":{"type":"string","enum":["off"]}},"required":["state"],"additionalProperties":false}}
]`)

var gapFormat = json.RawMessage(`{"type":"json_schema","name":"gap","strict":true,"schema":{"type":"object","properties":{
"sentence":{"type":"string","description":"The whole sentence he meant, in Spanish as a porteño says it, vos where it applies."},
"chunks":{"type":"array","items":{"type":"object","properties":{
"english":{"type":"string","description":"The English chunk exactly as he said it."},
"spanish":{"type":"string"},
"also":{"type":"array","items":{"type":"string"}},
"route":{"type":"string","enum":["thought","convert","import","new"]},
"thought":{"type":"string","description":"The found thought or conversion rule id that gets him there, or empty."},
"guessable":{"type":"boolean","description":"Whether he could reasonably guess it from English or Norwegian: a conversion rule he owns, a cognate, a shared root."},
"hint":{"type":"string","description":"For a guessable chunk, a hint that points at the route without giving it, such as: it's almost like in Norwegian. Empty otherwise."},
"ask":{"type":"string","description":"The short question that challenges him to produce it, ending on the English word or phrase, such as: how do you say different?"},
"link":{"type":"string","description":"A true cross-language link, or empty."},
"misses":{"type":"array","items":{"type":"object","properties":{"said":{"type":"string"},"cause":{"type":"string"},"ask":{"type":"string"}},"required":["said","cause","ask"],"additionalProperties":false}}},
"required":["english","spanish","also","route","thought","guessable","hint","ask","link","misses"],"additionalProperties":false}},
"aside":{"type":"string","description":"A false friend or a surprise worth one line, or empty."}},
"required":["sentence","chunks","aside"],"additionalProperties":false}}`)

func NewRosa(prompts, dir string) *Rosa {
	os.MkdirAll(filepath.Join(dir, "floor"), 0o700)
	return &Rosa{prompts: prompts, dir: dir, memory: OpenMemory(filepath.Join(dir, "transcript.jsonl")), learner: OpenLearner(filepath.Join(dir, "learner.json"))}
}

func (r *Rosa) read(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(r.prompts, name))
	if err != nil {
		return "", fmt.Errorf("cannot read voice/rosa/%s from ~/.agents", name)
	}
	return strings.TrimSpace(string(data)), nil
}

// Instructions are Rosa's live prompt, backend prompt and voice, from her own folder.
func (r *Rosa) Instructions() (live, backend, voice string, err error) {
	identity, err := r.read("identity.md")
	if err != nil {
		return "", "", "", err
	}
	if live, err = r.read("live.md"); err != nil {
		return "", "", "", err
	}
	if backend, err = r.read("backend.md"); err != nil {
		return "", "", "", err
	}
	if voice, err = r.read("voice"); err != nil {
		return "", "", "", err
	}
	return identity + "\n\n" + live, identity + "\n\n# Planner\n\n" + backend, voice, nil
}

// Graph is the thought graph, loaded on first use and again at the start of every call.
func (r *Rosa) Graph() (*Graph, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.graph != nil {
		return r.graph, nil
	}
	graph, err := LoadGraph(filepath.Join(r.prompts, "graph"), filepath.Join(r.dir, "thoughts.jsonl"))
	if err != nil {
		return nil, err
	}
	r.graph = graph
	return graph, nil
}

func (r *Rosa) planPath() string { return filepath.Join(r.dir, "plan.json") }

// Seed opens the call with the clock and what learner memory holds.
func (r *Rosa) Seed(now time.Time) []map[string]any {
	return []map[string]any{seedMessage("developer", "It is "+clock(now)+". "+r.learner.Summary(now))}
}

// Session follows one call: the class hands over the waiting plan, adjusts it to the opening chat,
// referees the floor and works out code-switched turns; the call is debriefed when it closes.
func (r *Rosa) Session(id string, conn Conn, backend *Backend, zone *time.Location, turnOff func() string) *Session {
	session := NewSession(id, conn, r.memory, &Recall{}, backend, func() string { return "" })
	session.Agent, session.Zone = "rosa", zone
	r.mu.Lock()
	r.graph = nil
	r.mu.Unlock()
	graph, err := r.Graph()
	if err != nil {
		log.Printf("rosa: %v", err)
		return session
	}
	class := newClass(graph, r.learner, filepath.Join(r.dir, "floor", id+".jsonl"), time.Now())
	gaps := &Backend{Client: backend.Client, URL: backend.URL, Auth: backend.Auth, Model: gapModel, Instructions: r.gapInstructions(), Tools: backend.Tools}
	class.onSwitch = func(said, about string) { r.gapPass(session, gaps, class, said, about) }
	class.prompted = func() { r.adjust(session, class) }
	class.exhausted = func() { r.more(session, class) }
	session.watch = class.watch
	session.preamble = func(now time.Time) []map[string]any {
		note := "It is " + clock(now) + ". This call started " + ago(now.Sub(session.started)) + ". " + class.Position()
		return []map[string]any{seedMessage("developer", note), seedMessage("developer", r.learner.Summary(now))}
	}
	r.mu.Lock()
	r.class, r.turnOff = class, turnOff
	r.mu.Unlock()
	go class.run(session)
	if plan, ok := loadPlan(r.planPath()); ok {
		class.SetPlan(plan)
	} else {
		go func() {
			ctx, cancel := context.WithTimeout(session.ctx, 4*time.Minute)
			defer cancel()
			plan, err := r.compose(ctx, backend, composeStart, "", time.Now())
			if err != nil {
				log.Printf("rosa: compose at start failed: %v", err)
				plan = Plan{Mode: "talk", Why: "No plan could be composed; talk with him and teach what his errors call for."}
			} else if err := savePlan(r.planPath(), plan); err != nil {
				log.Printf("rosa: %v", err)
			}
			class.SetPlan(plan)
		}()
	}
	go func() {
		<-session.Closed
		r.debrief(session, class)
	}()
	return session
}

// transcript is what was said on the call so far.
func (r *Rosa) transcript(session *Session) string {
	var b strings.Builder
	for _, message := range r.memory.Recent(time.Now(), 400) {
		if message.At >= session.started.UnixMilli() {
			who := "Lemon"
			if message.Role == "assistant" {
				who = "Rosa"
			}
			fmt.Fprintf(&b, "%s: %s\n", who, message.Text)
		}
	}
	return b.String()
}

// adjust fits the plan's items that have not reached the call yet to what Lemon said in the opening.
func (r *Rosa) adjust(session *Session, class *Class) {
	from, plan := class.Unhanded()
	if from >= len(plan.Items) && plan.Mode == "talk" {
		return
	}
	ctx, cancel := context.WithTimeout(session.ctx, 3*time.Minute)
	defer cancel()
	adjusted, err := r.adjustPlan(ctx, session.backend, plan, from, r.transcript(session), time.Now())
	if err != nil {
		log.Printf("rosa: adjust failed: %v", err)
		return
	}
	class.Replace(from, adjusted.Items, adjusted.Mode, adjusted.Why)
}

// more composes further items when the call's plan runs out; the call only ends when Lemon ends it.
func (r *Rosa) more(session *Session, class *Class) {
	ctx, cancel := context.WithTimeout(session.ctx, 4*time.Minute)
	defer cancel()
	plan, err := r.compose(ctx, session.backend, composeMore, r.transcript(session), time.Now())
	if err != nil {
		log.Printf("rosa: more items failed: %v", err)
	}
	class.Extend(plan.Items)
}

// gapPass works out a code-switched turn on the fast model and decides per chunk how Rosa handles
// it: words in his dictionary are challenged, guessable ones are challenged with a hint at the
// route, and the rest are given for him to build the sentence with.
func (r *Rosa) gapPass(session *Session, backend *Backend, class *Class, said, about string) {
	ctx, cancel := context.WithTimeout(session.ctx, 45*time.Second)
	defer cancel()
	graph, err := r.Graph()
	if err != nil {
		return
	}
	var found []string
	for _, thought := range graph.All() {
		if status := r.learner.Status(thought.ID); status == "found alone" || status == "found with help" {
			found = append(found, thought.ID+": "+thought.Title)
		}
	}
	input := []map[string]any{
		seedMessage("developer", "Thoughts he has found: "+truncate(strings.Join(found, "; "), 20000)),
		seedMessage("developer", fmt.Sprintf("He was %s and said: %q", about, said)),
	}
	text, _, err := backend.Complete(ctx, input, gapFormat, 1)
	if err != nil {
		log.Printf("rosa: gap pass failed: %v", err)
		return
	}
	var gap Gap
	if json.Unmarshal([]byte(text), &gap) != nil {
		return
	}
	gap.Said = said
	if !echoes(gap.Sentence, said) {
		log.Printf("rosa: gap pass ignored: %q is not what he said", gap.Sentence)
		return
	}
	for i, chunk := range gap.Chunks {
		gap.Chunks[i].Known = r.learner.Known(chunk.Spanish)
	}
	class.SetGap(gap)
}

// echoes is whether the gap pass's sentence keeps the Spanish he actually said: at least half of his
// Spanish words.
func echoes(sentence, said string) bool {
	in := map[string]bool{}
	for _, word := range words(sentence) {
		in[word] = true
	}
	total, kept := 0, 0
	for _, word := range words(said) {
		if spanish[word] || spanishEnding(word) {
			total++
			if in[word] {
				kept++
			}
		}
	}
	return total > 0 && kept*2 >= total
}

func (r *Rosa) gapInstructions() string {
	text, err := r.read("gap.md")
	if err != nil {
		return "Work out the English chunks in a Spanish learner's code-switched turn."
	}
	return text
}

// debrief records the call; a call of a few minutes is also reviewed into learner memory and leaves
// the next call's plan waiting.
func (r *Rosa) debrief(session *Session, class *Class) {
	now := time.Now()
	report := class.Finish(session.ID, now)
	if now.Sub(session.started) < debriefAfter {
		return
	}
	transcript := r.transcript(session)
	if transcript == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	input := []map[string]any{
		seedMessage("developer", "The call just ended. "+report),
		seedMessage("developer", r.learner.Summary(now)),
		seedMessage("developer", "Transcript of the call:\n"+truncate(transcript, 60000)),
		seedMessage("developer", "Review: with status, set the status of each thought the call covered that the referee report doesn't list: found alone only for a clean answer he built without a hint, including one he produced before it was taught; found with help when he fixed it after a hint or a question; introduced when he didn't get there. Near misses are never right. With remember, file each distinct wrong answer under its diagnosed cause, each learning habit you saw, each new durable fact about his life, and each cross-language link he reacted to and how. With words, add the Spanish words he was exposed to that are not in his dictionary yet, with how he got them. Skip what learner memory already holds. Then return say: two or three sentences for your next call with him: what landed, what to revisit, how the pace felt. details: empty."),
	}
	reply, err := session.backend.Answer(ctx, input)
	if err != nil {
		log.Printf("rosa: review failed: %v", err)
	} else {
		r.learner.Update(func(l *Learner) {
			for i := len(l.Calls) - 1; i >= 0; i-- {
				if l.Calls[i].Session == session.ID {
					l.Calls[i].Summary = truncate(reply.Say, 1000)
					break
				}
			}
		})
	}
	plan, err := r.compose(ctx, session.backend, composeNext, transcript, time.Now())
	if err != nil {
		log.Printf("rosa: compose failed: %v", err)
		return
	}
	if err := savePlan(r.planPath(), plan); err != nil {
		log.Printf("rosa: %v", err)
	}
}

// Call runs one of Rosa's backend tools.
func (r *Rosa) Call(_ context.Context, name, arguments string) (any, error) {
	var args struct {
		ID       string     `json:"id"`
		Query    string     `json:"query"`
		Why      string     `json:"why"`
		Targets  []Sentence `json:"targets"`
		Kind     string     `json:"kind"`
		Text     string     `json:"text"`
		Thought  string     `json:"thought"`
		Said     string     `json:"said"`
		Expected string     `json:"expected"`
		State    string     `json:"state"`
		Entries  []Word     `json:"entries"`
		Status   string     `json:"status"`
	}
	if arguments != "" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return nil, fmt.Errorf("invalid arguments")
		}
	}
	r.mu.Lock()
	class, turnOff := r.class, r.turnOff
	r.mu.Unlock()
	graph, err := r.Graph()
	if err != nil {
		return nil, err
	}
	switch name {
	case "learner":
		return map[string]any{"learner": json.RawMessage(r.learner.JSON())}, nil
	case "find":
		return map[string]any{"thoughts": graph.Find(args.Query, 30)}, nil
	case "thought":
		thought, ok := graph.Get(args.ID)
		if !ok {
			return nil, fmt.Errorf("no thought %q; use find", args.ID)
		}
		return map[string]any{"thought": thought, "status": r.learner.Status(args.ID)}, nil
	case "author":
		var thought Thought
		if err := json.Unmarshal([]byte(arguments), &thought); err != nil {
			return nil, fmt.Errorf("invalid thought")
		}
		if err := graph.Author(thought); err != nil {
			return nil, err
		}
		return map[string]any{"result": "Thought " + thought.ID + " is in the graph."}, nil
	case "remember":
		text := truncate(strings.TrimSpace(args.Text), 500)
		if text == "" {
			return nil, fmt.Errorf("nothing to remember")
		}
		err := r.learner.Update(func(l *Learner) {
			switch args.Kind {
			case "profile":
				l.Profile = append(l.Profile, text)
			case "habit":
				l.Habits = append(l.Habits, text)
			case "link":
				l.Links = append(l.Links, text)
			default:
				l.Errors = append(l.Errors, LearnerError{At: time.Now(), Thought: args.Thought, Said: truncate(args.Said, 300), Expected: truncate(args.Expected, 300), Cause: text})
			}
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"result": "Remembered."}, nil
	case "words":
		now := time.Now()
		err := r.learner.Update(func(l *Learner) {
			for _, entry := range args.Entries {
				l.expose(entry.Word, entry.How, now)
				key := strings.Join(words(entry.Word), "")
				if word, ok := l.Words[key]; ok {
					word.Sentence = truncate(entry.Sentence, 200)
					word.Links = append(word.Links, entry.Links...)
					if entry.Kind != "" && entry.Kind != "word" {
						word.Kind = entry.Kind
					}
					if entry.Gender != "" {
						word.Gender = entry.Gender
					}
					l.Words[key] = word
				}
			}
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"result": fmt.Sprintf("%d words in the dictionary.", len(args.Entries))}, nil
	case "status":
		if _, ok := graph.Get(args.ID); !ok {
			return nil, fmt.Errorf("no thought %q", args.ID)
		}
		now := time.Now()
		err := r.learner.Update(func(l *Learner) {
			record := l.Thoughts[args.ID]
			if record.At.IsZero() || now.Sub(record.At) > debriefAfter {
				record.Seen++
				if args.Status == "found alone" {
					record.Alone++
				}
			}
			record.Status, record.At = args.Status, now
			l.Thoughts[args.ID] = record
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"result": args.ID + " is " + args.Status + "."}, nil
	case "conversation":
		if args.State != "off" || turnOff == nil {
			return nil, fmt.Errorf("state must be off")
		}
		return turnOff(), nil
	}
	if class == nil {
		return nil, fmt.Errorf("no call is running")
	}
	switch name {
	case "plan":
		return map[string]any{"position": class.Position()}, nil
	case "teach":
		if _, ok := graph.Get(args.ID); !ok {
			return nil, fmt.Errorf("no thought %q; author it first", args.ID)
		}
		if len(args.Targets) == 0 {
			return nil, fmt.Errorf("write at least one target")
		}
		kind := "new"
		if r.learner.Status(args.ID) != "not introduced" {
			kind = "review"
		}
		return map[string]any{"result": class.Teach(PlanItem{Thought: args.ID, Kind: kind, Why: args.Why, Targets: args.Targets})}, nil
	case "talk":
		return map[string]any{"result": class.Talk()}, nil
	}
	return nil, fmt.Errorf("unknown tool: %s", name)
}
