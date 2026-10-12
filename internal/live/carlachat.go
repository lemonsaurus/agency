package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	carlaChatSeed = 40  // chat messages her backend reads
	carlaChatNews = 20  // updates held for her next text
	carlaChatLine = 600 // characters of one update she reads
)

// CarlaChatSchema is Carla's call tools that work in the chat. The call's conversation switch has
// nothing to turn off, and Discord stays with the call, which relays the phone's notifications.
var CarlaChatSchema = func() json.RawMessage {
	var tools []map[string]any
	json.Unmarshal(Schema, &tools)
	var chat []map[string]any
	for _, tool := range tools {
		if name := tool["name"].(string); name != "conversation" && name != "discord_recent" && name != "discord_reply" {
			chat = append(chat, tool)
		}
	}
	data, _ := json.Marshal(chat)
	return data
}()

// carlaChatTools are the tool names CarlaChatSchema offers.
var carlaChatTools = func() map[string]bool {
	var tools []struct {
		Name string `json:"name"`
	}
	json.Unmarshal(CarlaChatSchema, &tools)
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	return names
}()

// CarlaChat is texting with Carla: Lemon asks by text or voice note for anything he asks on a call,
// her backend runs the same tools, and the sessions he texted for report back as her texts.
type CarlaChat struct {
	*Thread
	// Backend is her chat backend; State is the dispatcher's; Recall is shared with the call; Zone is
	// Lemon's.
	Backend func() (*Backend, error)
	State   func() string
	Recall  *Recall
	Zone    func() *time.Location

	news []chatNews // guarded by Thread.mu
}

type chatNews struct {
	At     time.Time
	Update Update
}

func OpenCarlaChat(folder string) *CarlaChat {
	return &CarlaChat{Thread: OpenThread(folder, "carla"), State: func() string { return "" }, Recall: &Recall{}, Zone: func() *time.Location { return time.Local }}
}

// Report takes an update from work Lemon texted for: a spoken one gets a text, a quiet one is context
// for her next.
func (c *CarlaChat) Report(update Update) {
	c.mu.Lock()
	c.news = tail(append(c.news, chatNews{time.Now(), update}), carlaChatNews)
	c.mu.Unlock()
	if update.Spoken {
		c.poke()
	}
}

// Run answers his messages and texts session updates, until ctx ends.
func (c *CarlaChat) Run(ctx context.Context) {
	c.poke()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
		}
		for ctx.Err() == nil && (c.waiting() > 0 || c.spoken()) {
			c.turn(ctx)
		}
	}
}

// spoken is whether an update waits for a text.
func (c *CarlaChat) spoken() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, news := range c.news {
		if news.Update.Spoken {
			return true
		}
	}
	return false
}

// turn reads his messages, or the updates that arrived together, and sends one text.
func (c *CarlaChat) turn(ctx context.Context) {
	asked := ""
	if c.waiting() > 0 {
		if !c.settle(ctx, 0) {
			return
		}
		message := c.read()
		asked = message.Text
	} else {
		c.sleep(ctx, chatSettle)
		c.setDoing("typing")
	}
	defer c.setDoing("")
	c.mu.Lock()
	news := c.news
	c.news = nil
	c.mu.Unlock()
	backend, err := c.Backend()
	if err != nil {
		c.deliver(ChatMessage{From: "carla", Kind: "text", Text: "Something went wrong on the box: " + err.Error()})
		return
	}
	now := time.Now().In(c.Zone())
	closing := "Answer Lemon's latest messages now."
	if asked == "" {
		closing = "Text Lemon what the session updates above mean for him."
	}
	compose, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	reply, err := backend.Answer(compose, c.input(now, news, closing))
	if err != nil {
		log.Printf("carla chat: reply failed: %v", err)
		said := Failure(err, reply.Fetched)
		if asked != "" {
			c.Recall.Add(Exchange{At: now, Asked: asked, Fetched: reply.Fetched, Error: said})
		}
		c.deliver(ChatMessage{From: "carla", Kind: "text", Text: said})
		return
	}
	if asked != "" {
		c.Recall.Add(Exchange{At: now, Asked: asked, Fetched: reply.Fetched, Said: reply.Say, Details: reply.Details})
	}
	c.deliver(ChatMessage{From: "carla", Kind: "text", Text: strings.TrimSpace(reply.Say)})
}

// input is the clock, the dispatcher, recent results shared with the call, the chat so far and the
// session updates since her last text.
func (c *CarlaChat) input(now time.Time, news []chatNews, closing string) []map[string]any {
	note := "It is " + clock(now) + ". Lemon is texting you in the Carla app on his phone. " + c.State()
	input := []map[string]any{seedMessage("developer", strings.TrimSpace(note))}
	if brief := c.Recall.Brief(now); brief != "" {
		input = append(input, seedMessage("developer", brief))
	}
	input = append(input, seedMessage("developer", "The chat so far, oldest first. Each message starts with its time and number for you; never write those yourself."))
	for _, message := range c.history(carlaChatSeed) {
		line := fmt.Sprintf("[%s #%d] ", time.UnixMilli(message.At).In(now.Location()).Format("Mon 2 Jan 15:04"), message.ID)
		switch {
		case message.Kind == "voice" && message.Text == "":
			line += "(voice note that could not be transcribed; ask him to type it or say it again) "
		case message.Kind == "voice":
			line += "(voice note, speech recognition of what he said) "
		}
		line += message.Text
		if emoji := message.Reactions["lemon"]; emoji != "" {
			line += " (Lemon reacted " + emoji + ")"
		}
		role := "user"
		if message.From == "carla" {
			role = "assistant"
		}
		input = append(input, seedMessage(role, line))
	}
	if len(news) > 0 {
		var b strings.Builder
		b.WriteString("Updates from the sessions working on what he texted for, oldest first:")
		for _, item := range news {
			kind := "for him"
			if !item.Update.Spoken {
				kind = "context"
			}
			fmt.Fprintf(&b, "\n- %s (%s): %s", item.At.In(now.Location()).Format("15:04"), kind, truncate(item.Update.Content, carlaChatLine))
		}
		input = append(input, seedMessage("developer", b.String()))
	}
	return append(input, seedMessage("developer", closing))
}
