package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Conn is a sideband connection to a GPT-Live session: one JSON event per message.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, event []byte) error
	Close() error
}

// Session follows one voice conversation over its sideband: transcripts into memory, delegations
// to the backend, and updates back into the call. The dispatcher outlives it in the Manager.
type Session struct {
	ID      string
	Agent   string // carla or rosa
	conn    Conn
	memory  *Memory
	recall  *Recall
	backend *Backend
	state   func() string
	started time.Time
	last    time.Time      // when Lemon last spoke before this call
	Zone    *time.Location // Lemon's, from the phone
	// preamble opens every backend request: clock, state and recent results.
	preamble func(now time.Time) []map[string]any
	// watch sees transcript fragments and delegations with their session-timeline times.
	watch func(kind, text string, startMS, endMS int64)

	ctx    context.Context
	cancel context.CancelFunc
	reason string    // how the session ended
	heard  time.Time // the last transcript fragment, either way
	writes sync.Mutex
	events int
	mu     sync.Mutex
	closed bool
	Closed chan struct{}
}

func NewSession(id string, conn Conn, memory *Memory, recall *Recall, backend *Backend, state func() string) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{ID: id, Agent: "carla", conn: conn, memory: memory, recall: recall, backend: backend, state: state, started: time.Now(), last: memory.Last(), Zone: time.Local, ctx: ctx, cancel: cancel, Closed: make(chan struct{})}
	s.preamble = s.carla
	return s
}

// Run reads sideband events until the session closes.
func (s *Session) Run() {
	defer s.finish()
	flush := time.NewTicker(15 * time.Second)
	defer flush.Stop()
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-flush.C:
				s.memory.Flush(false)
			}
		}
	}()
	for {
		data, err := s.conn.Read(s.ctx)
		if err != nil {
			s.end("sideband closed: " + err.Error())
			return
		}
		var event struct {
			Type       string `json:"type"`
			Delta      string `json:"delta"`
			StartMS    int64  `json:"start_ms"`
			EndMS      int64  `json:"end_ms"`
			OffsetMS   int64  `json:"offset_ms"`
			Reason     string `json:"reason"`
			Delegation struct {
				ID     string `json:"id"`
				Target string `json:"target"`
			} `json:"delegation"`
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &event) != nil {
			continue
		}
		if s.watch != nil {
			switch event.Type {
			case "session.input_transcript.delta", "session.output_transcript.delta":
				s.watch(event.Type, event.Delta, event.StartMS, event.EndMS)
			case "session.delegation.created":
				s.watch(event.Type, "", event.OffsetMS, event.OffsetMS)
			}
		}
		switch event.Type {
		case "session.input_transcript.delta":
			s.memory.Add("user", event.Delta, time.Now())
			s.mu.Lock()
			s.heard = time.Now()
			s.mu.Unlock()
		case "session.output_transcript.delta":
			s.memory.Add("assistant", event.Delta, time.Now())
			s.mu.Lock()
			s.heard = time.Now()
			s.mu.Unlock()
		case "session.delegation.created":
			if event.Delegation.Target == "client" {
				go s.delegate(event.Delegation.ID)
			}
		case "error":
			log.Printf("live: %s %s", event.Error.Code, event.Error.Message)
			if s.watch != nil {
				s.watch("error", event.Error.Code+": "+event.Error.Message, 0, 0)
			}
		case "session.closed":
			log.Printf("live: session %s closed (%s)", s.ID, event.Reason)
			s.end("OpenAI closed it: " + event.Reason)
			return
		}
	}
}

// delegate answers one request from the voice model with the recent transcript and recent results
// as context, and leaves the facts behind the answer in the conversation for follow-ups.
func (s *Session) delegate(id string) {
	// The delegation can arrive before the last transcript fragments; let them land.
	time.Sleep(400 * time.Millisecond)
	now := time.Now().In(s.Zone)
	input := s.preamble(now)
	marked := false
	asked := ""
	for _, message := range s.memory.Recent(now, 24) {
		if message.Role == "user" {
			asked = message.Text
		}
		if !marked && message.At >= s.started.UnixMilli() {
			input = append(input, seedMessage("developer", "(This call starts here.)"))
			marked = true
		}
		text := message.Text
		if since := now.Sub(time.UnixMilli(message.At)); since >= pauseGap {
			text = "(" + ago(since) + ") " + text
		}
		input = append(input, seedMessage(message.Role, text))
	}
	input = append(input, seedMessage("developer", "Handle the latest thing Lemon asked for in the transcript above. Reply with what to say to him now."))
	reply, err := s.backend.Answer(s.ctx, input)
	if err != nil {
		s.append("session.commentary.append", id, "Something went wrong on the box: "+err.Error())
		return
	}
	s.recall.Add(Exchange{At: now, Asked: asked, Fetched: reply.Fetched, Said: reply.Say, Details: reply.Details})
	if strings.TrimSpace(reply.Details) != "" {
		s.append("session.thinking.append", "", "Facts behind your next answer, as of "+now.Format("15:04")+", for follow-ups; not to read out: "+reply.Details)
	}
	s.append("session.commentary.append", id, reply.Say)
}

// carla opens Carla's backend requests: the clock, how long Lemon was away, the dispatcher, and
// what recent delegations already fetched.
func (s *Session) carla(now time.Time) []map[string]any {
	note := "It is " + clock(now) + ". This call started " + ago(now.Sub(s.started)) + ". " + away(s.started, s.last)
	if !fresh(s.started, s.last) && !s.recall.Checked(s.started) {
		note += " Check the live state with your tools before reporting progress or what a session is doing."
	}
	input := []map[string]any{seedMessage("developer", note+" "+s.state())}
	if brief := s.recall.Brief(now); brief != "" {
		input = append(input, seedMessage("developer", brief))
	}
	return input
}

// Emit sends an update into the conversation.
func (s *Session) Emit(update Update) {
	kind := "session.thinking.append"
	if update.Spoken {
		kind = "session.commentary.append"
	}
	s.append(kind, "", update.Content)
}

func (s *Session) append(kind, delegation, content string) {
	s.writes.Lock()
	defer s.writes.Unlock()
	s.events++
	event := map[string]any{"type": kind, "event_id": fmt.Sprintf("box_%d", s.events), "content": truncate(content, updateLimit)}
	if delegation == "" {
		event["delegation_id"] = nil
	} else {
		event["delegation_id"] = delegation
	}
	data, _ := json.Marshal(event)
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	if err := s.conn.Write(ctx, data); err != nil {
		log.Printf("live: append failed: %v", err)
	}
}

// Instruct appends a session-wide instruction, e.g. words Lemon said while reconnecting.
func (s *Session) Instruct(content string) {
	s.append("session.instructions.append", "", content)
}

func (s *Session) finish() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.memory.Flush(true)
	s.conn.Close()
	close(s.Closed)
}

// Close ends the sideband; the Live session itself is closed by the phone.
func (s *Session) Close() {
	s.end("closed by the box: the phone hung up or a new call replaced it")
	s.finish()
}

// end records the first reason the session ended, and logs it with how long the line had been quiet.
func (s *Session) end(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason != "" {
		return
	}
	quiet := "nothing was ever said"
	if !s.heard.IsZero() {
		quiet = "last speech " + ago(time.Since(s.heard))
	}
	s.reason = reason + "; " + quiet
	log.Printf("live: %s session %s ended after %s: %s", s.Agent, s.ID, span(time.Since(s.started)), s.reason)
}

// Ended is how the session ended, once it has.
func (s *Session) Ended() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}
