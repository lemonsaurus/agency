package live

import (
	"fmt"
	"sync"
	"time"
)

// Discord holds messages the phone was notified about and replies the phone still has to send.
// Everything stays in memory on the box.
type Discord struct {
	mu       sync.Mutex
	messages []DiscordMessage
	replies  []DiscordReply
	nextID   int
	emit     func(Update)
}

type DiscordMessage struct {
	ID       int    `json:"id"`
	PhoneID  int    `json:"phone_id"`
	Sender   string `json:"from"`
	Place    string `json:"in"`
	Text     string `json:"text"`
	At       int64  `json:"at"`
	CanReply bool   `json:"can_reply"`
}

type DiscordReply struct {
	ID      int    `json:"id"`
	PhoneID int    `json:"phone_id"`
	Text    string `json:"text"`
	State   string `json:"state"` // pending, sent, failed
	Error   string `json:"error,omitempty"`
}

func NewDiscord(emit func(Update)) *Discord {
	return &Discord{emit: emit}
}

// Post records messages from the phone; new ones are read out.
func (d *Discord) Post(messages []DiscordMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, message := range messages {
		found := false
		for i := range d.messages {
			if d.messages[i].PhoneID == message.PhoneID {
				d.messages[i].CanReply = message.CanReply
				found = true
			}
		}
		if found {
			continue
		}
		d.nextID++
		message.ID = d.nextID
		d.messages = append(d.messages, message)
		if len(d.messages) > 30 {
			d.messages = d.messages[1:]
		}
		note := ""
		if !message.CanReply {
			note = " (no reply possible from here)"
		}
		d.emit(Update{true, fmt.Sprintf("Discord message %d from %s in %s: %s%s", message.ID, message.Sender, message.Place, truncate(message.Text, 500), note)})
	}
}

func (d *Discord) Recent() []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	start := 0
	if len(d.messages) > 8 {
		start = len(d.messages) - 8
	}
	var listed []map[string]any
	for _, message := range d.messages[start:] {
		listed = append(listed, map[string]any{
			"id": message.ID, "from": message.Sender, "in": message.Place, "text": truncate(message.Text, 600),
			"minutes_ago": int(time.Since(time.UnixMilli(message.At)).Minutes()), "can_reply": message.CanReply,
		})
	}
	return listed
}

// Reply queues the text for the phone, which owns Discord's notification action.
func (d *Discord) Reply(id int, text string) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.messages {
		message := &d.messages[i]
		if message.ID != id {
			continue
		}
		if !message.CanReply {
			return nil, fmt.Errorf("Discord has dismissed that message, so the reply can't go from here; Lemon replies from the phone when stopped")
		}
		message.CanReply = false
		d.replies = append(d.replies, DiscordReply{ID: id, PhoneID: message.PhoneID, Text: text, State: "pending"})
		return map[string]any{"result": fmt.Sprintf("Reply to %s in %s is on its way through the phone.", message.Sender, message.Place)}, nil
	}
	return nil, fmt.Errorf("no such Discord message; list recent messages first")
}

// Pending is what the phone should send now.
func (d *Discord) Pending() []DiscordReply {
	d.mu.Lock()
	defer d.mu.Unlock()
	var pending []DiscordReply
	for _, reply := range d.replies {
		if reply.State == "pending" {
			pending = append(pending, reply)
		}
	}
	return pending
}

// Done records the phone's outcome for a reply.
func (d *Discord) Done(id int, errText string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.replies {
		reply := &d.replies[i]
		if reply.ID != id || reply.State != "pending" {
			continue
		}
		if errText == "" {
			reply.State = "sent"
		} else {
			reply.State, reply.Error = "failed", errText
			d.emit(Update{true, "The Discord reply did not go through: " + truncate(errText, 200)})
		}
	}
}
