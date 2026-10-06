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

// Rosa is the Spanish tutor voice: her own persona, prompts, voice, transcript memory, learner
// memory and tools. She never loads Carla's persona and never sees the box's panes.
type Rosa struct {
	prompts string // ~/.agents/voice/rosa
	dir     string // ~/.agents/run/agency/rosa
	memory  *Memory
	learner *LearnerStore

	mu      sync.Mutex
	class   *Class
	turnOff func() string
}

// RosaSchema is the function list Rosa's backend sees.
var RosaSchema = json.RawMessage(`[
{"type":"function","name":"lesson","description":"Where today's lesson stands: every thought with Lemon's status, the thought being taught, the open prompt, and the outcome of each sentence so far.","parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}},
{"type":"function","name":"thought","description":"One thought of today's lesson in full: explanations, Norwegian import, target sentences and known near misses.","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}},
{"type":"function","name":"learner","description":"Everything learner memory holds about Lemon: profile facts, thought statuses, errors by cause, habits, sentences already used, pacing samples and past calls.","parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}},
{"type":"function","name":"remember","description":"Add to learner memory. kind profile: a durable fact about Lemon's life to build sentences from. kind error: a wrong answer filed under its diagnosed cause (text is the cause; give thought, said and expected). kind habit: a learning habit seen, such as reciting tables, guessing, or inventing mnemonics. Never store anything Lemon asks you not to.","parameters":{"type":"object","properties":{"kind":{"type":"string","enum":["profile","error","habit"]},"text":{"type":"string"},"thought":{"type":"string"},"said":{"type":"string"},"expected":{"type":"string"}},"required":["kind","text","thought","said","expected"],"additionalProperties":false}},
{"type":"function","name":"move","description":"Move today's lesson to another thought by id, when Lemon wants to go back, skip ahead, or the pace calls for it. Its notes reach the call at once.","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}},
{"type":"function","name":"conversation","description":"End the call when Lemon is done or asks to stop. The phone hangs up after your next sentence, so say a short goodbye.","parameters":{"type":"object","properties":{"state":{"type":"string","enum":["off"]}},"required":["state"],"additionalProperties":false}}
]`)

func NewRosa(prompts, dir string) *Rosa {
	os.MkdirAll(filepath.Join(dir, "floor"), 0o700)
	return &Rosa{prompts: prompts, dir: dir, memory: OpenMemory(filepath.Join(dir, "transcript.jsonl")), learner: OpenLearner(filepath.Join(dir, "learner.json"))}
}

// Instructions are Rosa's live prompt, backend prompt and voice, from her own folder.
func (r *Rosa) Instructions() (live, backend, voice string, err error) {
	read := func(name string) (string, error) {
		data, err := os.ReadFile(filepath.Join(r.prompts, name))
		if err != nil {
			return "", fmt.Errorf("cannot read voice/rosa/%s from ~/.agents", name)
		}
		return strings.TrimSpace(string(data)), nil
	}
	identity, err := read("identity.md")
	if err != nil {
		return "", "", "", err
	}
	if live, err = read("live.md"); err != nil {
		return "", "", "", err
	}
	if backend, err = read("backend.md"); err != nil {
		return "", "", "", err
	}
	if voice, err = read("voice"); err != nil {
		return "", "", "", err
	}
	return identity + "\n\n" + live, identity + "\n\n# Lesson backend\n\n" + backend, voice, nil
}

// Seed opens the call with the clock and what learner memory holds.
func (r *Rosa) Seed(now time.Time) []map[string]any {
	return []map[string]any{seedMessage("developer", "It is "+clock(now)+". "+r.learner.Summary(now))}
}

// Session follows one lesson call: the class referees and logs the floor, and the call is debriefed
// into learner memory when it closes.
func (r *Rosa) Session(id string, conn Conn, backend *Backend, zone *time.Location, turnOff func() string) *Session {
	session := NewSession(id, conn, r.memory, &Recall{}, backend, func() string { return "" })
	session.Agent, session.Zone = "rosa", zone
	lessons, err := LoadLessons(filepath.Join(r.prompts, "course"))
	if err != nil {
		log.Printf("rosa: %v", err)
		return session
	}
	lesson := lessons[0]
	for _, candidate := range lessons {
		if done := r.done(candidate); !done {
			lesson = candidate
			break
		}
	}
	class := newClass(lesson, r.learner, filepath.Join(r.dir, "floor", id+".jsonl"), time.Now())
	session.watch = class.watch
	session.preamble = func(now time.Time) []map[string]any {
		note := "It is " + clock(now) + ". This lesson call started " + ago(now.Sub(session.started)) + ". " + class.Position()
		return []map[string]any{seedMessage("developer", note), seedMessage("developer", r.learner.Summary(now))}
	}
	r.mu.Lock()
	r.class, r.turnOff = class, turnOff
	r.mu.Unlock()
	go class.run(session)
	class.open()
	go func() {
		<-session.Closed
		r.debrief(session, class)
	}()
	return session
}

// done is whether Lemon found every thought of a lesson alone.
func (r *Rosa) done(lesson Lesson) bool {
	for _, thought := range lesson.Thoughts {
		if r.learner.Status(thought.ID) != "found alone" {
			return false
		}
	}
	return true
}

// debrief records the call and asks the backend to file errors, habits and profile facts.
func (r *Rosa) debrief(session *Session, class *Class) {
	now := time.Now()
	report := class.Finish(session.ID, now)
	var transcript strings.Builder
	for _, message := range r.memory.Recent(now, 400) {
		if message.At >= session.started.UnixMilli() {
			fmt.Fprintf(&transcript, "%s: %s\n", message.Role, message.Text)
		}
	}
	if transcript.Len() == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	input := []map[string]any{
		seedMessage("developer", "The lesson call just ended. "+report),
		seedMessage("developer", r.learner.Summary(now)),
		seedMessage("developer", "Transcript of the call (user is Lemon, assistant is you):\n"+truncate(transcript.String(), 60000)),
		seedMessage("developer", "Debrief: file each distinct wrong answer under its diagnosed cause, each learning habit you saw, and each new durable fact Lemon told you about his life, with remember. Skip what learner memory already holds. Then return say: two or three sentences for your next call with him: what landed, what to revisit, how the pace felt. details: empty."),
	}
	reply, err := session.backend.Answer(ctx, input)
	if err != nil {
		log.Printf("rosa: debrief failed: %v", err)
		return
	}
	r.learner.Update(func(l *Learner) {
		for i := len(l.Calls) - 1; i >= 0; i-- {
			if l.Calls[i].Session == session.ID {
				l.Calls[i].Summary = truncate(reply.Say, 1000)
				break
			}
		}
	})
}

// Call runs one of Rosa's backend tools.
func (r *Rosa) Call(_ context.Context, name, arguments string) (any, error) {
	var args struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		Text     string `json:"text"`
		Thought  string `json:"thought"`
		Said     string `json:"said"`
		Expected string `json:"expected"`
		State    string `json:"state"`
	}
	if arguments != "" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return nil, fmt.Errorf("invalid arguments")
		}
	}
	r.mu.Lock()
	class, turnOff := r.class, r.turnOff
	r.mu.Unlock()
	switch name {
	case "learner":
		return map[string]any{"learner": json.RawMessage(r.learner.JSON())}, nil
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
			default:
				l.Errors = append(l.Errors, LearnerError{At: time.Now(), Thought: args.Thought, Said: truncate(args.Said, 300), Expected: truncate(args.Expected, 300), Cause: text})
			}
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"result": "Remembered."}, nil
	case "conversation":
		if args.State != "off" || turnOff == nil {
			return nil, fmt.Errorf("state must be off")
		}
		return turnOff(), nil
	}
	if class == nil {
		return nil, fmt.Errorf("no lesson is running")
	}
	switch name {
	case "lesson":
		return map[string]any{"overview": class.lesson.Overview(r.learner.Status), "position": class.Position()}, nil
	case "thought":
		for _, thought := range class.lesson.Thoughts {
			if thought.ID == args.ID {
				return map[string]any{"thought": thought}, nil
			}
		}
		return nil, fmt.Errorf("no thought %q in lesson %s", args.ID, class.lesson.ID)
	case "move":
		result, err := class.Move(args.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"result": result}, nil
	}
	return nil, fmt.Errorf("unknown tool: %s", name)
}
