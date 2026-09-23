package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Backend answers one delegation with the Responses API, running tools through the dispatcher
// until the model produces text. Stateless: every round resends the input plus the model's output.
type Backend struct {
	Client       *http.Client
	URL          string
	Key          string
	Model        string
	Instructions string
	Tools        func(ctx context.Context, name, arguments string) (any, error)
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

// Answer runs the loop and returns the spoken-style result.
func (b *Backend) Answer(ctx context.Context, input []map[string]any) (string, error) {
	items := make([]json.RawMessage, 0, len(input)+8)
	for _, item := range input {
		data, _ := json.Marshal(item)
		items = append(items, data)
	}
	for round := 0; round < 8; round++ {
		outputs, err := b.request(ctx, items)
		if err != nil {
			return "", err
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
				item, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": output.CallID, "output": string(payload)})
				items = append(items, item)
			}
		}
		if calls == 0 {
			if len(text) == 0 {
				return "", fmt.Errorf("the backend produced no answer")
			}
			return strings.Join(text, "\n"), nil
		}
	}
	return "", fmt.Errorf("the backend kept calling tools without answering")
}

func (b *Backend) request(ctx context.Context, input []json.RawMessage) ([]responseOutput, error) {
	body, _ := json.Marshal(map[string]any{
		"model":               b.Model,
		"instructions":        b.Instructions,
		"input":               input,
		"tools":               Schema,
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
		"reasoning":           map[string]string{"effort": "low"},
		"store":               false,
		"include":             []string{"reasoning.encrypted_content"},
	})
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+b.Key)
	request.Header.Set("Content-Type", "application/json")
	response, err := b.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("backend request failed")
	}
	defer response.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("backend reply unreadable")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(reply, &failure)
		return nil, fmt.Errorf("backend returned HTTP %d: %s", response.StatusCode, truncate(strings.ReplaceAll(failure.Error.Message, b.Key, "[key]"), 300))
	}
	var parsed struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(reply, &parsed); err != nil {
		return nil, fmt.Errorf("invalid backend reply")
	}
	outputs := make([]responseOutput, 0, len(parsed.Output))
	for _, raw := range parsed.Output {
		var output responseOutput
		if err := json.Unmarshal(raw, &output); err != nil {
			continue
		}
		output.Raw = raw
		outputs = append(outputs, output)
	}
	return outputs, nil
}
