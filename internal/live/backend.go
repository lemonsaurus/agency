package live

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Backend answers one delegation with the Responses API on the ChatGPT plan, running tools through
// the dispatcher until the model produces text. Stateless: every round resends the input plus the
// model's output.
type Backend struct {
	Client       *http.Client
	URL          string
	Auth         func(ctx context.Context) (token, account string, err error)
	Model        string
	Instructions string
	Tools        func(ctx context.Context, name, arguments string) (any, error)
}

// Reply is the backend's answer: what to say now, the facts behind it for follow-ups, and what its
// tools returned.
type Reply struct {
	Say     string    `json:"say"`
	Details string    `json:"details"`
	Fetched []Fetched `json:"-"`
}

var replyFormat = json.RawMessage(`{"type":"json_schema","name":"reply","strict":true,"schema":{"type":"object","properties":{
"say":{"type":"string","description":"What Carla says to Lemon now: one short spoken-style paragraph, no IDs."},
"details":{"type":"string","description":"The facts behind it that Lemon may follow up on, as compact notes: session task names, states, counts, times, findings. Not read aloud. Under 1000 characters; empty when there are none."}},
"required":["say","details"],"additionalProperties":false}}`)

type responseOutput struct {
	Type      string          `json:"type"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Content   []responsePart  `json:"content"`
	Raw       json.RawMessage `json:"-"`
}

type responsePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Answer runs the loop and returns the reply with every tool call it made.
func (b *Backend) Answer(ctx context.Context, input []map[string]any) (Reply, error) {
	items := make([]json.RawMessage, 0, len(input)+8)
	for _, item := range input {
		data, _ := json.Marshal(item)
		items = append(items, data)
	}
	var fetched []Fetched
	for round := 0; round < 8; round++ {
		outputs, err := b.request(ctx, items)
		if err != nil {
			return Reply{}, err
		}
		var text []string
		calls := 0
		for _, output := range outputs {
			items = append(items, output.Raw)
			switch output.Type {
			case "message":
				for _, part := range output.Content {
					if part.Type == "output_text" && strings.TrimSpace(part.Text) != "" {
						text = append(text, part.Text)
					}
				}
			case "function_call":
				calls++
				result, err := b.Tools(ctx, output.Name, output.Arguments)
				var payload []byte
				if err != nil {
					payload, _ = json.Marshal(map[string]string{"error": err.Error() + ". Do not retry automatically."})
				} else {
					payload, _ = json.Marshal(result)
				}
				fetched = append(fetched, Fetched{output.Name, output.Arguments, string(payload)})
				item, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": output.CallID, "output": string(payload)})
				items = append(items, item)
			}
		}
		if calls == 0 {
			var reply Reply
			if json.Unmarshal([]byte(strings.Join(text, "")), &reply) != nil || strings.TrimSpace(reply.Say) == "" {
				return Reply{}, fmt.Errorf("the backend produced no answer")
			}
			reply.Fetched = fetched
			return reply, nil
		}
	}
	return Reply{}, fmt.Errorf("the backend kept calling tools without answering")
}

func (b *Backend) request(ctx context.Context, input []json.RawMessage) ([]responseOutput, error) {
	token, account, err := b.Auth(ctx)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{
		"model":               b.Model,
		"instructions":        b.Instructions,
		"input":               input,
		"tools":               Schema,
		"text":                map[string]any{"format": replyFormat},
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
		"reasoning":           map[string]string{"effort": "low"},
		"store":               false,
		"stream":              true,
		"include":             []string{"reasoning.encrypted_content"},
	})
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("chatgpt-account-id", account)
	request.Header.Set("originator", "agency")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	response, err := b.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("backend request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		reply, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		var failure struct {
			Detail string `json:"detail"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(reply, &failure)
		message := failure.Error.Message + failure.Detail
		return nil, fmt.Errorf("backend returned HTTP %d: %s", response.StatusCode, truncate(strings.ReplaceAll(message, token, "[token]"), 300))
	}
	// The stream's completed event carries no output; the items arrive one by one as output_item.done.
	var outputs []responseOutput
	events := bufio.NewScanner(response.Body)
	events.Buffer(make([]byte, 64<<10), 8<<20)
	for events.Scan() {
		data, found := strings.CutPrefix(events.Text(), "data: ")
		if !found {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Item     json.RawMessage `json:"item"`
			Message  string          `json:"message"`
			Response struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		switch event.Type {
		case "response.output_item.done":
			var output responseOutput
			if json.Unmarshal(event.Item, &output) == nil {
				output.Raw = event.Item
				outputs = append(outputs, output)
			}
		case "response.completed":
			return outputs, nil
		case "response.failed", "response.incomplete", "error":
			return nil, fmt.Errorf("backend failed: %s", truncate(event.Message+event.Response.Error.Message, 300))
		}
	}
	return nil, fmt.Errorf("backend stream ended before the response completed")
}
