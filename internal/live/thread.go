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

// Thread is a WhatsApp-style chat between Lemon and one agent: chat.jsonl, chat-state.json and media
// in chat/, all in the agent's folder. The phone syncs it by change counter.
type Thread struct {
	who    string
	folder string
	dir    string // media files
	path   string
	// Hear transcribes his voice notes; Wake tells the phone to sync.
	Hear func(ctx context.Context, path string) (string, error)
	Wake func()

	mu       sync.Mutex
	messages []ChatMessage
	state    chatState
	doing    string
	polled   time.Time
	kick     chan struct{}
}

// OpenThread loads who's chat with Lemon from folder.
func OpenThread(folder, who string) *Thread {
	t := &Thread{who: who, folder: folder, dir: filepath.Join(folder, "chat"), path: filepath.Join(folder, "chat.jsonl"), kick: make(chan struct{}, 1)}
	data, _ := os.ReadFile(t.path)
	for _, line := range strings.Split(string(data), "\n") {
		var message ChatMessage
		if strings.TrimSpace(line) != "" && json.Unmarshal([]byte(line), &message) == nil && message.ID != 0 {
			t.messages = append(t.messages, message)
		}
	}
	if raw, err := os.ReadFile(t.statePath()); err == nil {
		json.Unmarshal(raw, &t.state)
	}
	return t
}

func (t *Thread) statePath() string { return filepath.Join(t.folder, "chat-state.json") }

// save writes the messages and state; the caller holds the lock.
func (t *Thread) save() {
	os.MkdirAll(t.folder, 0o700)
	t.messages = tail(t.messages, chatKeep)
	var b strings.Builder
	for _, message := range t.messages {
		data, _ := json.Marshal(message)
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(t.path+".tmp", []byte(b.String()), 0o600); err == nil {
		os.Rename(t.path+".tmp", t.path)
	}
	state, _ := json.Marshal(t.state)
	if err := os.WriteFile(t.statePath()+".tmp", state, 0o600); err == nil {
		os.Rename(t.statePath()+".tmp", t.statePath())
	}
}

// add appends a message as the next change; the caller holds the lock.
func (t *Thread) add(message ChatMessage) ChatMessage {
	t.state.Next++
	t.state.Rev++
	message.ID, message.Rev, message.At = t.state.Next, t.state.Rev, time.Now().UnixMilli()
	t.messages = append(t.messages, message)
	t.save()
	return message
}

// Sync returns what changed after rev and marks the phone as looking at the chat.
func (t *Thread) Sync(rev int64) ChatSync {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.polled = time.Now()
	sync := ChatSync{Rev: t.state.Rev, Doing: t.doing, Seen: t.state.Seen, Messages: []ChatMessage{}}
	sync.Online = t.doing != "" || time.Since(time.UnixMilli(t.state.Seen)) < chatOnline
	for _, message := range t.messages {
		if message.Rev > rev {
			sync.Messages = append(sync.Messages, message)
		}
	}
	sync.Messages = tail(sync.Messages, chatSync)
	return sync
}

// Send files a message from Lemon and wakes the agent.
func (t *Thread) Send(text string) (ChatMessage, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return ChatMessage{}, fmt.Errorf("write something first")
	}
	t.mu.Lock()
	message := t.add(ChatMessage{From: "lemon", Kind: "text", Text: truncate(text, 4000)})
	t.polled = time.Now()
	t.mu.Unlock()
	t.poke()
	return message, nil
}

// Dir is where voice notes and photos live; his voice notes are written there before SendVoice.
func (t *Thread) Dir() string { return t.dir }

// SendVoice files a voice note from Lemon, transcribed so the agent can read what he said, and wakes it.
func (t *Thread) SendVoice(ctx context.Context, path string, seconds float64, peaks []float64) (ChatMessage, error) {
	if filepath.Dir(filepath.Clean(path)) != t.dir {
		return ChatMessage{}, fmt.Errorf("voice notes live in %s", t.dir)
	}
	if _, err := os.Stat(path); err != nil {
		return ChatMessage{}, err
	}
	text := ""
	if t.Hear != nil {
		heard, err := t.Hear(ctx, path)
		if err != nil {
			log.Printf("%s chat: transcription failed: %v", t.who, err)
		}
		text = heard
	}
	t.mu.Lock()
	message := t.add(ChatMessage{From: "lemon", Kind: "voice", Text: truncate(text, 4000), Media: path, Seconds: seconds, Peaks: tail(peaks, 64)})
	t.polled = time.Now()
	t.mu.Unlock()
	t.poke()
	return message, nil
}

// React sets who's emoji on a message; empty removes it.
func (t *Thread) React(id int, who, emoji string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.messages {
		if t.messages[i].ID != id {
			continue
		}
		if t.messages[i].Reactions == nil {
			t.messages[i].Reactions = map[string]string{}
		}
		if emoji == "" {
			delete(t.messages[i].Reactions, who)
		} else {
			t.messages[i].Reactions[who] = truncate(emoji, 16)
		}
		t.state.Rev++
		t.messages[i].Rev = t.state.Rev
		t.save()
		return nil
	}
	return fmt.Errorf("no message %d", id)
}

func (t *Thread) poke() {
	select {
	case t.kick <- struct{}{}:
	default:
	}
}

func (t *Thread) setDoing(doing string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.doing = doing
	t.state.Seen = time.Now().UnixMilli()
}

// waiting is the newest message of his the agent has not answered, 0 when there is none.
func (t *Thread) waiting() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.messages) - 1; i >= 0; i-- {
		if t.messages[i].From == "lemon" {
			if t.messages[i].ID > t.state.Handled {
				return t.messages[i].ID
			}
			return 0
		}
	}
	return 0
}

// settle waits until he has stopped sending for a moment, and at least pause from now.
func (t *Thread) settle(ctx context.Context, pause time.Duration) bool {
	start := time.Now()
	for {
		t.mu.Lock()
		last := time.UnixMilli(t.messages[len(t.messages)-1].At)
		t.mu.Unlock()
		wait := paced(max(time.Until(start.Add(pause)), time.Until(last.Add(chatSettle))))
		if wait <= 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
	}
}

// read marks all his messages read and handled, shows the agent typing, and returns his last message.
func (t *Thread) read() ChatMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	var last ChatMessage
	now := time.Now().UnixMilli()
	for i := range t.messages {
		if t.messages[i].From == "lemon" {
			last = t.messages[i]
			if t.messages[i].Read == 0 {
				t.state.Rev++
				t.messages[i].Read, t.messages[i].Rev = now, t.state.Rev
			}
		}
	}
	t.state.Handled = last.ID
	t.doing, t.state.Seen = "typing", now
	t.save()
	return last
}

func (t *Thread) sleep(ctx context.Context, d time.Duration) {
	if d = paced(d); d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (t *Thread) fromLemon(id int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, message := range t.messages {
		if message.ID == id {
			return message.From == "lemon"
		}
	}
	return false
}

// history is the last n messages.
func (t *Thread) history(n int) []ChatMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return tail(append([]ChatMessage(nil), t.messages...), n)
}

// deliver adds one of the agent's messages and wakes the phone unless the chat is open on it.
func (t *Thread) deliver(message ChatMessage) {
	if message.Text == "" && message.Media == "" {
		return
	}
	t.mu.Lock()
	t.add(message)
	t.state.Seen = time.Now().UnixMilli()
	t.save()
	open := time.Since(t.polled) < chatPolling
	t.mu.Unlock()
	if !open && t.Wake != nil {
		go t.Wake()
	}
}

func (t *Thread) store(kind, ext string, data []byte) (string, error) {
	os.MkdirAll(t.dir, 0o700)
	path := filepath.Join(t.dir, fmt.Sprintf("%s-%d%s", kind, time.Now().UnixMilli(), ext))
	return path, os.WriteFile(path, data, 0o600)
}
