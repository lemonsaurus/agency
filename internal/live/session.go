package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	conn    Conn
	memory  *Memory
	backend *Backend
	state   func() string

	ctx    context.Context
	cancel context.CancelFunc
	writes sync.Mutex
	events int
	mu     sync.Mutex
	closed bool
	Closed chan struct{}
}

func NewSession(id string, conn Conn, memory *Memory, backend *Backend, state func() string) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{ID: id, conn: conn, memory: memory, backend: backend, state: state, ctx: ctx, cancel: cancel, Closed: make(chan struct{})}
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
			return
		}
		var event struct {
			Type       string `json:"type"`
			Delta      string `json:"delta"`
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
		switch event.Type {
		case "session.input_transcript.delta":
			s.memory.Add("user", event.Delta, time.Now())
		case "session.output_transcript.delta":
			s.memory.Add("assistant", event.Delta, time.Now())
		case "session.delegation.created":
			if event.Delegation.Target == "client" {
				go s.delegate(event.Delegation.ID)
			}
		case "error":
			log.Printf("live: %s %s", event.Error.Code, event.Error.Message)
		case "session.closed":
			log.Printf("live: session %s closed (%s)", s.ID, event.Reason)
			return
		}
	}
}

// delegate answers one request from the voice model with the recent transcript as context.
func (s *Session) delegate(id string) {
	// The delegation can arrive before the last transcript fragments; let them land.
	time.Sleep(400 * time.Millisecond)
	input := []map[string]any{seedMessage("developer", s.state())}
	for _, message := range s.memory.Recent(time.Now(), 24) {
		input = append(input, seedMessage(message.Role, message.Text))
	}
	input = append(input, seedMessage("developer", "Handle the latest thing Lemon asked for in the transcript above. Reply with what to say to him now."))
	text, err := s.backend.Answer(s.ctx, input)
	if err != nil {
		text = "Something went wrong on the box: " + err.Error()
	}
	s.append("session.commentary.append", id, truncate(text, updateLimit))
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
	s.finish()
}
