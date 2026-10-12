package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	chatKeep    = 3000 // messages kept on disk
	chatSync    = 200  // most messages one sync returns
	chatSeed    = 80   // chat messages the reply model reads
	chatOnline  = 45 * time.Second
	chatPolling = 8 * time.Second // a phone that synced this recently has the chat open
	chatSettle  = 3500 * time.Millisecond
	chatCPS     = 14.0 // how fast she types, characters per second
)

// paced is how long her human pauses really take; tests make them instant.
var paced = func(d time.Duration) time.Duration { return d }

// ChatMessage is one WhatsApp-style message between Lemon and Rosa. Kind is text, voice or photo;
// Text is the message, the voice note's words, or the photo's caption, and Scene what the photo
// shows. Read is when Rosa read one of his. Rev is the change counter value of its last change.
type ChatMessage struct {
	ID        int               `json:"id"`
	From      string            `json:"from"`
	Kind      string            `json:"kind"`
	Text      string            `json:"text"`
	Media     string            `json:"media,omitempty"`
	Seconds   float64           `json:"seconds,omitempty"`
	Peaks     []float64         `json:"peaks,omitempty"`
	Scene     string            `json:"scene,omitempty"`
	At        int64             `json:"at"`
	Read      int64             `json:"read,omitempty"`
	Reactions map[string]string `json:"reactions,omitempty"`
	Rev       int64             `json:"rev"`
}

type chatState struct {
	Rev     int64     `json:"rev"`
	Next    int       `json:"next"`
	Seen    int64     `json:"seen"`    // when she was last online
	Handled int       `json:"handled"` // the last message of his she has answered
	Nudge   time.Time `json:"nudge"`   // when she may next text first
}

// ChatSync is what the phone polls: changes after its revision, what she is doing and when she was
// last online.
type ChatSync struct {
	Rev      int64         `json:"rev"`
	Doing    string        `json:"doing"`
	Online   bool          `json:"online"`
	Seen     int64         `json:"seen"`
	Messages []ChatMessage `json:"messages"`
}

// Chat is texting with Rosa: her pace, voice notes in her call voice, photos from her day, and
// texting first now and then. It shares her learner memory, canon and call transcript.
type Chat struct {
	*Thread
	rosa *Rosa
	// Backend is her texting model with chat tools; Record renders a voice note in her voice; Take
	// draws a photo; Zone is Lemon's; Calling is whether she is on a call with him.
	Backend func() *Backend
	Record  func(ctx context.Context, script string) (VoiceNote, error)
	Take    func(ctx context.Context, scene string, look []byte) ([]byte, error)
	Zone    func() *time.Location
	Calling func() bool
}

var chatFormat = json.RawMessage(`{"type":"json_schema","name":"texts","strict":true,"schema":{"type":"object","properties":{
"react":{"type":"object","description":"An emoji reaction to one of his messages, or id 0 and an empty emoji for none.","properties":{"id":{"type":"integer"},"emoji":{"type":"string"}},"required":["id","emoji"],"additionalProperties":false},
"messages":{"type":"array","description":"What you send now, in order. Empty when a reaction is the whole reply.","items":{"type":"object","properties":{
"kind":{"type":"string","enum":["text","voice","photo"]},
"text":{"type":"string","description":"text: the message. voice: exactly what you say. photo: a short caption, or empty."},
"scene":{"type":"string","description":"photo: what the camera sees, concretely: place, light, what is in frame, what you wear if you are in it. Empty otherwise."},
"selfie":{"type":"boolean","description":"photo: you are in it."}},
"required":["kind","text","scene","selfie"],"additionalProperties":false}}},
"required":["react","messages"],"additionalProperties":false}}`)

// ChatSchema is the part of Rosa's tools that work outside a call.
var ChatSchema = func() json.RawMessage {
	var tools []map[string]any
	json.Unmarshal(RosaSchema, &tools)
	keep := map[string]bool{"find": true, "thought": true, "author": true, "learner": true, "remember": true, "words": true, "status": true,
		"etymology": true, "canon": true, "canon_add": true, "canon_told": true}
	var chat []map[string]any
	for _, tool := range tools {
		if keep[tool["name"].(string)] {
			chat = append(chat, tool)
		}
	}
	data, _ := json.Marshal(chat)
	return data
}()

func OpenChat(rosa *Rosa) *Chat {
	return &Chat{Thread: OpenThread(rosa.dir, "rosa"), rosa: rosa, Zone: func() *time.Location { return time.Local }, Calling: func() bool { return false }}
}

// Run answers his messages and texts first now and then, until ctx ends.
func (c *Chat) Run(ctx context.Context) {
	nudge := time.NewTicker(10 * time.Minute)
	defer nudge.Stop()
	c.poke()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
		case <-nudge.C:
			c.nudge(ctx, time.Now())
			continue
		}
		for c.waiting() > 0 && ctx.Err() == nil {
			c.reply(ctx)
		}
	}
}

// reply reads his messages at a human pace, composes, and sends.
func (c *Chat) reply(ctx context.Context) {
	c.mu.Lock()
	away := time.Since(time.UnixMilli(c.state.Seen)) > 2*time.Minute
	c.mu.Unlock()
	pause := time.Duration(2000+rand.IntN(3000)) * time.Millisecond
	if away {
		pause = time.Duration(6+rand.IntN(25)) * time.Second
	}
	// He often sends several in a row: read once he has stopped for a moment.
	if !c.settle(ctx, pause) {
		return
	}
	c.read()
	c.answer(ctx, "Reply to Lemon's latest messages now.")
}

// answer composes her turn with the given closing note and delivers it.
func (c *Chat) answer(ctx context.Context, note string) {
	defer c.setDoing("")
	if c.Backend == nil {
		return
	}
	backend := c.Backend()
	if backend == nil {
		return
	}
	started := time.Now()
	compose, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	text, _, err := backend.Complete(compose, c.input(time.Now().In(c.Zone()), note), chatFormat, 12)
	if err != nil {
		log.Printf("rosa chat: reply failed, trying once more: %v", err)
		c.sleep(ctx, 20*time.Second)
		text, _, err = backend.Complete(compose, c.input(time.Now().In(c.Zone()), note), chatFormat, 12)
	}
	if err != nil {
		log.Printf("rosa chat: reply failed: %v", err)
		return
	}
	var reply struct {
		React struct {
			ID    int    `json:"id"`
			Emoji string `json:"emoji"`
		} `json:"react"`
		Messages []struct {
			Kind   string `json:"kind"`
			Text   string `json:"text"`
			Scene  string `json:"scene"`
			Selfie bool   `json:"selfie"`
		} `json:"messages"`
	}
	if json.Unmarshal([]byte(text), &reply) != nil {
		log.Printf("rosa chat: unreadable reply")
		return
	}
	if reply.React.ID != 0 && reply.React.Emoji != "" && c.fromLemon(reply.React.ID) {
		c.React(reply.React.ID, "rosa", reply.React.Emoji)
	}
	for i, message := range reply.Messages {
		if ctx.Err() != nil {
			return
		}
		switch message.Kind {
		case "voice":
			c.voice(ctx, message.Text)
		case "photo":
			c.photo(ctx, message.Scene, message.Selfie, message.Text)
		default:
			// The first message was being typed while she thought.
			typing := time.Duration(float64(len([]rune(message.Text))) / chatCPS * float64(time.Second))
			if i == 0 {
				typing -= time.Since(started)
			} else {
				c.setDoing("")
				c.sleep(ctx, time.Duration(500+rand.IntN(1200))*time.Millisecond)
				c.setDoing("typing")
				typing = min(max(typing, 1200*time.Millisecond), 8*time.Second)
			}
			c.sleep(ctx, min(typing, 8*time.Second))
			c.deliver(ChatMessage{From: "rosa", Kind: "text", Text: strings.TrimSpace(message.Text)})
		}
	}
}

// voice records a voice note in her call voice; if that fails she types it instead.
func (c *Chat) voice(ctx context.Context, script string) {
	script = strings.TrimSpace(script)
	if script == "" {
		return
	}
	c.setDoing("recording")
	if err := c.recorded(ctx, script); err != nil {
		log.Printf("rosa chat: voice note failed: %v", err)
	} else {
		c.setDoing("typing")
		return
	}
	c.setDoing("typing")
	c.deliver(ChatMessage{From: "rosa", Kind: "text", Text: script})
}

func (c *Chat) recorded(ctx context.Context, script string) error {
	if c.Record == nil {
		return fmt.Errorf("no voice")
	}
	note, err := c.Record(ctx, script)
	if err != nil {
		return err
	}
	path, err := c.store("voice", ".wav", note.WAV)
	if err != nil {
		return err
	}
	c.deliver(ChatMessage{From: "rosa", Kind: "voice", Text: script, Media: path, Seconds: note.Duration.Seconds(), Peaks: note.Peaks})
	return nil
}

// photo draws a photo from her day; without one, only the caption goes.
func (c *Chat) photo(ctx context.Context, scene string, selfie bool, caption string) {
	caption = strings.TrimSpace(caption)
	err := c.snapped(ctx, scene, selfie, caption)
	if err == nil {
		return
	}
	log.Printf("rosa chat: photo failed: %v", err)
	c.deliver(ChatMessage{From: "rosa", Kind: "text", Text: caption})
}

func (c *Chat) snapped(ctx context.Context, scene string, selfie bool, caption string) error {
	if c.Take == nil || strings.TrimSpace(scene) == "" {
		return fmt.Errorf("no camera or no scene")
	}
	var look []byte
	if selfie {
		look, _ = os.ReadFile(filepath.Join(c.rosa.prompts, "look.jpg"))
	}
	picture, err := c.Take(ctx, PhotoPrompt(scene, look != nil), look)
	if err != nil {
		return err
	}
	path, err := c.store("photo", ".jpg", picture)
	if err != nil {
		return err
	}
	c.deliver(ChatMessage{From: "rosa", Kind: "photo", Text: caption, Media: path, Scene: scene})
	return nil
}

// learner memory, her canon, the waiting plan, and the chat so far.
func (c *Chat) input(now time.Time, closing string) []map[string]any {
	r := c.rosa
	history := c.history(chatSeed)
	note := "It is " + clock(now) + ". You are texting with Lemon on WhatsApp."
	if c.Calling() {
		note += " You are on a call with him right now as well."
	} else if last := r.memory.Last(); !last.IsZero() {
		note += " Your last call with him ended " + ago(now.Sub(last)) + "."
	}
	input := []map[string]any{seedMessage("developer", note), seedMessage("developer", r.learner.Summary(now)), seedMessage("developer", r.canon.Summary(now))}
	if plan, ok := loadPlan(r.planPath()); ok && len(plan.Items) > 0 {
		var items []string
		for _, item := range tail(plan.Items, 6) {
			items = append(items, strings.TrimSpace(item.Type+" "+item.Thought+" "+item.Question+": "+item.Why))
		}
		input = append(input, seedMessage("developer", "What you planned to teach him next, for texts too: "+strings.Join(items, "; ")))
	}
	if call := r.memory.Tail(30); len(call) > 0 && now.Sub(time.UnixMilli(call[len(call)-1].At)) < 7*24*time.Hour {
		var b strings.Builder
		for _, message := range call {
			who := "Lemon"
			if message.Role == "assistant" {
				who = "You"
			}
			fmt.Fprintf(&b, "%s: %s\n", who, message.Text)
		}
		input = append(input, seedMessage("developer", "The end of your last call with him, spoken:\n"+truncate(b.String(), 8000)))
	}
	input = append(input, seedMessage("developer", "The chat so far, oldest first. Each message starts with its time and number for you; never write those yourself."))
	for _, message := range history {
		line := fmt.Sprintf("[%s #%d] ", time.UnixMilli(message.At).In(now.Location()).Format("Mon 2 Jan 15:04"), message.ID)
		switch {
		case message.Kind == "voice" && message.From == "lemon" && message.Text == "":
			line += "(voice note that could not be transcribed; ask him to say it again or type it) "
		case message.Kind == "voice" && message.From == "lemon":
			line += "(voice note, speech recognition of what he said) "
		case message.Kind == "voice":
			line += "(voice note) "
		case message.Kind == "photo":
			line += "(photo: " + message.Scene + ") "
		}
		line += message.Text
		for who, emoji := range message.Reactions {
			if who == "lemon" {
				line += " (Lemon reacted " + emoji + ")"
			} else {
				line += " (you reacted " + emoji + ")"
			}
		}
		role := "user"
		if message.From == "rosa" {
			role = "assistant"
		}
		input = append(input, seedMessage(role, line))
	}
	if n := len(history); n > 0 {
		input = append(input, seedMessage("developer", "His last message was "+ago(now.Sub(time.UnixMilli(history[n-1].At)))+" ago."))
	}
	return append(input, seedMessage("developer", closing))
}

// Recent is the chat of the last days as notes for a call, newest last, at most 2500 characters.
func (c *Chat) Recent(now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var lines []string
	for _, message := range tail(c.messages, 40) {
		at := time.UnixMilli(message.At)
		if now.Sub(at) > 3*24*time.Hour {
			continue
		}
		who := "Lemon"
		if message.From == "rosa" {
			who = "You"
		}
		text := message.Text
		switch message.Kind {
		case "voice":
			text = "(voice note) " + text
		case "photo":
			text = "(photo of " + message.Scene + ") " + text
		}
		lines = append(lines, fmt.Sprintf("[%s] %s: %s\n", at.In(now.Location()).Format("Mon 15:04"), who, truncate(text, 400)))
	}
	size, start := 0, len(lines)
	for start > 0 && size+len(lines[start-1]) <= 2500 {
		size += len(lines[start-1])
		start--
	}
	return strings.Join(lines[start:], "")
}

// nudge texts him first when she is due to: during the day, when neither of them has said anything
// for a while; then the next one is one to three days off.
func (c *Chat) nudge(ctx context.Context, now time.Time) {
	now = now.In(c.Zone())
	c.mu.Lock()
	due := c.state.Nudge
	lastChat := time.Time{}
	if n := len(c.messages); n > 0 {
		lastChat = time.UnixMilli(c.messages[n-1].At)
	}
	c.mu.Unlock()
	switch {
	case due.IsZero():
		c.schedule(now)
		return
	case now.Before(due) || now.Hour() < 10 || now.Hour() >= 22 || c.Calling():
		return
	}
	if now.Sub(lastChat) < 12*time.Hour || now.Sub(c.rosa.memory.Last()) < 12*time.Hour {
		c.schedule(now)
		return
	}
	c.schedule(now)
	c.setDoing("typing")
	c.answer(ctx, "You are texting him first, unprompted. He has said nothing since the chat above. A short opener from your day and a hook, and ask whether he wants to practise a bit.")
}

func (c *Chat) schedule(now time.Time) {
	day := now.AddDate(0, 0, 1+rand.IntN(3))
	at := time.Date(day.Year(), day.Month(), day.Day(), 11, 0, 0, 0, now.Location()).Add(time.Duration(rand.IntN(570)) * time.Minute)
	c.mu.Lock()
	c.state.Nudge = at
	c.save()
	c.mu.Unlock()
}
