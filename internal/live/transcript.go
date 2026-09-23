package live

import (
	"encoding/json"
	"sort"
	"strings"
)

// Turn is one conversational step in a pane, as the phone's chat shows it.
type Turn struct {
	Kind     string // lemon, carla, tool, skill
	At       int64
	Text     string
	Thinking string
	Name     string // tool or skill name
	Summary  string // tool argument of note
	Result   string
	Error    bool
}

type transcriptEntry struct {
	Role   string `json:"role"`
	At     int64  `json:"at"`
	Blocks []struct {
		Type  string                     `json:"type"`
		Text  string                     `json:"text"`
		ID    string                     `json:"id"`
		Name  string                     `json:"name"`
		Error bool                       `json:"error"`
		Args  map[string]json.RawMessage `json:"args"`
	} `json:"blocks"`
}

var fileArguments = []string{"path", "file", "command", "pattern", "url"}

// ParseTranscript mirrors the phone's Transcript.parse so narration matches what the chat shows.
func ParseTranscript(raw []json.RawMessage) []Turn {
	entries := make([]transcriptEntry, 0, len(raw))
	for _, item := range raw {
		var entry transcriptEntry
		if json.Unmarshal(item, &entry) == nil {
			entries = append(entries, entry)
		}
	}
	type result struct {
		at   int64
		text string
		err  bool
	}
	results := map[string]result{}
	for _, entry := range entries {
		if entry.Role != "toolResult" {
			continue
		}
		for _, block := range entry.Blocks {
			if block.Type == "toolResult" {
				results[block.ID] = result{entry.At, block.Text, block.Error}
			}
		}
	}
	var turns []Turn
	for _, entry := range entries {
		switch entry.Role {
		case "user":
			var parts []string
			for _, block := range entry.Blocks {
				if block.Type == "text" {
					parts = append(parts, block.Text)
				}
			}
			if text := strings.TrimSpace(strings.Join(parts, "\n")); text != "" {
				turns = append(turns, Turn{Kind: "lemon", At: entry.At, Text: text})
			}
		case "assistant":
			var text, thinking []string
			for _, block := range entry.Blocks {
				switch block.Type {
				case "text":
					text = append(text, block.Text)
				case "thinking":
					thinking = append(thinking, block.Text)
				}
			}
			if t, th := strings.TrimSpace(strings.Join(text, "\n")), strings.TrimSpace(strings.Join(thinking, "\n\n")); t != "" || th != "" {
				turns = append(turns, Turn{Kind: "carla", At: entry.At, Text: t, Thinking: th})
			}
			for _, block := range entry.Blocks {
				if block.Type != "toolCall" {
					continue
				}
				summary := ""
				for _, key := range fileArguments {
					var value string
					if raw, ok := block.Args[key]; ok && json.Unmarshal(raw, &value) == nil && value != "" {
						summary = value
						break
					}
				}
				if strings.HasSuffix(summary, "/SKILL.md") && block.Name == "read" {
					name := strings.TrimSuffix(summary, "/SKILL.md")
					turns = append(turns, Turn{Kind: "skill", At: entry.At, Name: name[strings.LastIndex(name, "/")+1:]})
					continue
				}
				turn := Turn{Kind: "tool", At: entry.At, Name: block.Name, Summary: truncate(summary, 200)}
				if r, ok := results[block.ID]; ok {
					turn.At, turn.Result, turn.Error = r.at, r.text, r.err
				}
				turns = append(turns, turn)
			}
		}
	}
	sort.SliceStable(turns, func(i, j int) bool { return turns[i].At < turns[j].At })
	return turns
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
