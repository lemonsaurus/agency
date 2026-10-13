package live

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
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
	Schema       json.RawMessage
	Tools        func(ctx context.Context, name, arguments string) (any, error)
	// Mutates names the tools that change something; one answer runs each with the same arguments
	// at most once, whatever the model asks.
	Mutates func(name string) bool
}

// Reply is the backend's answer: what to say now, the facts behind it for follow-ups, and what its
// tools returned.
type Reply struct {
	Say     string    `json:"say"`
	Details string    `json:"details"`
	Fetched []Fetched `json:"-"`
}

var replyFormat = json.RawMessage(`{"type":"json_schema","name":"reply","strict":true,"schema":{"type":"object","properties":{
"say":{"type":"string","description":"What to say to Lemon now: one short spoken-style paragraph, no IDs."},
"details":{"type":"string","description":"The facts behind it that Lemon may follow up on, as compact notes: session task names, states, counts, times, findings. Not read aloud. Under 1000 characters; empty when there are none."}},
"required":["say","details"],"additionalProperties":false}}`)

// retryDelays are the pauses before resending a request the endpoint failed on its side. The tool
// calls already made stay in the input, so a resend never runs a tool twice.
var retryDelays = []time.Duration{2 * time.Second, 5 * time.Second}

// transientError is the endpoint failing on its side: overloaded, a 5xx or 429, or a dropped stream.
type transientError struct{ error }

func (e transientError) Unwrap() error { return e.error }

// Transient is whether err is the endpoint failing on its side.
func Transient(err error) bool {
	var transient transientError
	return errors.As(err, &transient)
}

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

// Answer runs the loop and returns the reply with every tool call it made. A failed answer still
// carries the tool calls that ran before the failure.
func (b *Backend) Answer(ctx context.Context, input []map[string]any) (Reply, error) {
	return b.AnswerWithin(ctx, input, 8)
}

// AnswerWithin is Answer with room for more tool rounds, for work like a call review.
func (b *Backend) AnswerWithin(ctx context.Context, input []map[string]any, rounds int) (Reply, error) {
	text, fetched, err := b.Complete(ctx, input, replyFormat, rounds)
	if err != nil {
		return Reply{Fetched: fetched}, err
	}
	var reply Reply
	if json.Unmarshal([]byte(text), &reply) != nil || strings.TrimSpace(reply.Say) == "" {
		return Reply{Fetched: fetched}, fmt.Errorf("the backend produced no answer")
	}
	reply.Fetched = fetched
	return reply, nil
}

// Complete runs the tool loop for up to rounds requests and returns the final text in format, and
// every tool call it made, also when it fails.
func (b *Backend) Complete(ctx context.Context, input []map[string]any, format json.RawMessage, rounds int) (string, []Fetched, error) {
	items := make([]json.RawMessage, 0, len(input)+8)
	for _, item := range input {
		data, _ := json.Marshal(item)
		items = append(items, data)
	}
	var fetched []Fetched
	for round := 0; round < rounds; round++ {
		outputs, err := b.send(ctx, items, format)
		if err != nil {
			return "", fetched, err
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
				if b.Mutates != nil && b.Mutates(output.Name) && ran(fetched, output.Name, output.Arguments) {
					item, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": output.CallID,
						"output": `{"error":"This exact call already ran in this answer and was not repeated. Use its result above."}`})
					items = append(items, item)
					continue
				}
				result, err := b.Tools(ctx, output.Name, output.Arguments)
				var payload []byte
				var lookup LookupError
				switch {
				case errors.As(err, &lookup):
					payload, _ = json.Marshal(map[string]string{"error": err.Error() + ". Only this lookup failed: nothing changed and no session's work failed. Looking again is safe."})
				case err != nil:
					payload, _ = json.Marshal(map[string]string{"error": err.Error() + ". Do not retry automatically; it may have taken effect."})
				default:
					payload, _ = json.Marshal(result)
				}
				fetched = append(fetched, Fetched{output.Name, output.Arguments, string(payload)})
				item, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": output.CallID, "output": string(payload)})
				items = append(items, item)
			}
		}
		if calls == 0 {
			return strings.Join(text, ""), fetched, nil
		}
	}
	return "", fetched, fmt.Errorf("the backend kept calling tools without answering")
}

type endpointError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// reason is every code and message a failure event carries, or the raw event when it carries none.
func reason(raw string, parts ...any) string {
	var found []string
	for _, part := range parts {
		switch part := part.(type) {
		case string:
			found = append(found, part)
		case endpointError:
			found = append(found, part.Type, part.Code, part.Message)
		}
	}
	var said []string
	for _, part := range found {
		if part = strings.TrimSpace(part); part != "" && !slices.Contains(said, part) {
			said = append(said, part)
		}
	}
	if len(said) == 0 {
		return "the endpoint gave no reason: " + truncate(raw, 200)
	}
	return truncate(strings.Join(said, ": "), 300)
}

// ran is whether this answer already ran the tool with these arguments.
func ran(fetched []Fetched, name, arguments string) bool {
	for _, call := range fetched {
		if call.Tool == name && call.Arguments == arguments {
			return true
		}
	}
	return false
}

// send is request, resent after each of retryDelays while the endpoint fails on its side.
func (b *Backend) send(ctx context.Context, input []json.RawMessage, format json.RawMessage) ([]responseOutput, error) {
	for attempt := 0; ; attempt++ {
		outputs, err := b.request(ctx, input, format)
		if err == nil || !Transient(err) || attempt == len(retryDelays) {
			return outputs, err
		}
		log.Printf("live: backend request failed, resending in %s: %v", retryDelays[attempt], err)
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(retryDelays[attempt]):
		}
	}
}

func (b *Backend) request(ctx context.Context, input []json.RawMessage, format json.RawMessage) ([]responseOutput, error) {
	token, account, err := b.Auth(ctx)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{
		"model":        b.Model,
		"instructions": b.Instructions,
		"input":        input,
		"text":         map[string]any{"format": format},
		"reasoning":    map[string]string{"effort": "low"},
		"store":        false,
		"stream":       true,
		"include":      []string{"reasoning.encrypted_content"},
	}
	if b.Schema != nil {
		fields["tools"], fields["tool_choice"], fields["parallel_tool_calls"] = b.Schema, "auto", false
	}
	body, _ := json.Marshal(fields)
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
		if ctx.Err() != nil {
			return nil, fmt.Errorf("backend request failed")
		}
		return nil, transientError{fmt.Errorf("backend request failed")}
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
		err := fmt.Errorf("backend returned HTTP %d: %s", response.StatusCode, truncate(strings.ReplaceAll(message, token, "[token]"), 300))
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
			return nil, transientError{err}
		}
		return nil, err
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
			Code     string          `json:"code"`
			Message  string          `json:"message"`
			Error    endpointError   `json:"error"`
			Response struct {
				Error             endpointError `json:"error"`
				IncompleteDetails struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
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
		case "response.failed", "error":
			log.Printf("live: backend %s event: %s", event.Type, truncate(data, 2000))
			return nil, transientError{fmt.Errorf("backend failed: %s", reason(data, event.Code, event.Message, event.Error, event.Response.Error))}
		case "response.incomplete":
			log.Printf("live: backend %s event: %s", event.Type, truncate(data, 2000))
			return nil, fmt.Errorf("backend failed: response incomplete: %s", reason(data, event.Response.IncompleteDetails.Reason, event.Response.Error))
		}
	}
	return nil, transientError{fmt.Errorf("backend stream ended before the response completed")}
}
