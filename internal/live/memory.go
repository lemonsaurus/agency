package live

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Memory is what was said in recent voice conversations, one JSON line per message, so a new
// session can pick up where the last one stopped. Same-speaker deltas merge into one message.
type Memory struct {
	mu       sync.Mutex
	path     string
	messages []memoryMessage
	written  int
}

type memoryMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
	At   int64  `json:"at"`
}

const (
	memoryGap     = 20 * time.Second
	memoryWindow  = 3 * time.Hour
	memoryKeep    = 200
	memoryMaxSeed = 60
	memoryMsgMax  = 2000
	memorySeedMax = 12000
)

func OpenMemory(path string) *Memory {
	m := &Memory{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(data), "\n") {
		var message memoryMessage
		if strings.TrimSpace(line) != "" && json.Unmarshal([]byte(line), &message) == nil {
			m.messages = append(m.messages, message)
		}
	}
	if len(m.messages) > memoryKeep {
		m.messages = m.messages[len(m.messages)-memoryKeep:]
	}
	m.written = len(m.messages)
	return m
}

func (m *Memory) Add(role, delta string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := at.UnixMilli()
	if n := len(m.messages); n > 0 && m.messages[n-1].Role == role && ms-m.messages[n-1].At < memoryGap.Milliseconds() {
		last := &m.messages[n-1]
		last.Text = truncate(last.Text+delta, memoryMsgMax)
		last.At = ms
	} else {
		m.messages = append(m.messages, memoryMessage{role, delta, ms})
	}
	for len(m.messages) > memoryKeep {
		m.messages = m.messages[1:]
		if m.written > 0 {
			m.written--
		}
	}
}

// Flush appends completed messages to the log, and the open one too when closing.
func (m *Memory) Flush(closing bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	end := len(m.messages) - 1
	if closing {
		end = len(m.messages)
	}
	if end <= m.written {
		return nil
	}
	var lines strings.Builder
	for _, message := range m.messages[m.written:end] {
		data, _ := json.Marshal(message)
		lines.Write(data)
		lines.WriteByte('\n')
	}
	m.written = end
	if m.path == "" {
		return nil
	}
	file, err := os.OpenFile(m.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(lines.String())
	return err
}

// Recent returns the last messages within the window as text lines for a backend prompt.
func (m *Memory) Recent(now time.Time, limit int) []memoryMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var recent []memoryMessage
	for _, message := range m.messages {
		if now.UnixMilli()-message.At < memoryWindow.Milliseconds() && strings.TrimSpace(message.Text) != "" {
			recent = append(recent, message)
		}
	}
	if len(recent) > limit {
		recent = recent[len(recent)-limit:]
	}
	return recent
}

// Seed is the startup history for GPT-Live: the previous conversation behind a developer note.
func (m *Memory) Seed(now time.Time) []map[string]any {
	recent := m.Recent(now, memoryMaxSeed)
	budget := memorySeedMax
	start := len(recent)
	for start > 0 && budget-len(recent[start-1].Text) >= 0 {
		budget -= len(recent[start-1].Text)
		start--
	}
	kept := recent[start:]
	if len(kept) == 0 {
		return nil
	}
	minutes := int(now.Sub(time.UnixMilli(kept[len(kept)-1].At)).Minutes())
	if minutes < 1 {
		minutes = 1
	}
	ago := fmt.Sprintf("%d minutes ago", minutes)
	if minutes >= 60 {
		ago = fmt.Sprintf("%d hours ago", minutes/60)
	}
	seed := []map[string]any{seedMessage("developer", "This is what you and Lemon said in your previous voice conversation, which ended "+ago+". Continue naturally if he picks a thread back up; do not recap it unprompted.")}
	for _, message := range kept {
		seed = append(seed, seedMessage(message.Role, message.Text))
	}
	return seed
}

func seedMessage(role, text string) map[string]any {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return map[string]any{"type": "message", "role": role, "content": []map[string]any{{"type": kind, "text": text}}}
}
