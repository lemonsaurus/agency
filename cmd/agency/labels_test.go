package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLabelMessage(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		pane    string
		label   string
		write   bool
		wantErr bool
	}{
		{name: "read current"},
		{name: "read target", args: []string{"--pane", "%7"}, pane: "%7"},
		{name: "write current", args: []string{"--", "Task name"}, label: "Task name", write: true},
		{name: "write target", args: []string{"--pane", "%7", "--", "Task name"}, pane: "%7", label: "Task name", write: true},
		{name: "clear", args: []string{"--", ""}, write: true},
		{name: "option-like label", args: []string{"--", "--pane"}, label: "--pane", write: true},
		{name: "missing separator", args: []string{"Task name"}, wantErr: true},
		{name: "missing label", args: []string{"--"}, wantErr: true},
		{name: "missing target", args: []string{"--pane"}, wantErr: true},
		{name: "invalid target", args: []string{"--pane", "project"}, wantErr: true},
		{name: "extra argument", args: []string{"--", "one", "two"}, wantErr: true},
		{name: "multiline", args: []string{"--", "one\ntwo"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			message, write, err := labelMessage(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("labelMessage = %q, %v", message, err)
			}
			if tt.wantErr {
				return
			}
			var payload struct {
				Pane  string
				Label *string
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(message, "label:")), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Pane != tt.pane || write != tt.write || (payload.Label != nil) != tt.write {
				t.Fatalf("payload=%+v, write=%v", payload, write)
			}
			if tt.write && *payload.Label != tt.label {
				t.Fatalf("label=%q", *payload.Label)
			}
		})
	}
}

func TestSpawnMessagesIncludeLabel(t *testing.T) {
	label := "Cloud \"Harness\" Setup"
	for _, window := range []string{"", "cloud"} {
		for _, message := range []string{
			spawnAgentMessage(window, "manager", "pi", "/tmp", label),
			spawnCommandMessage(window, "manager", "pi --no-session", "/tmp", label),
		} {
			parts := strings.SplitN(message, ":", 2)
			var payload spawnPayload
			if err := json.Unmarshal([]byte(parts[1]), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Label != label || payload.Role != "manager" || payload.Window != window || payload.Dir != "/tmp" {
				t.Fatalf("spawn payload = %+v", payload)
			}
		}
	}
	if got := spawnAgentMessage("", "", "pi", "/tmp", "Human task"); !strings.Contains(got, `"label":"Human task"`) {
		t.Fatalf("human label omitted: %s", got)
	}
}
