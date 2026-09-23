// Package live runs Carla's voice backend on the box: a sideband on the GPT-Live session, a
// dispatcher that queues work for Pi panes, and narration of their progress back into the call.
package live

import (
	"context"
	"encoding/json"
)

// Box is what the dispatcher needs from the daemon: panes, projects, and the prompt bridge.
type Box interface {
	Panes(ctx context.Context) ([]Pane, error)
	Projects() ([]Project, error)
	Spawn(ctx context.Context, dir, label string) error
	Kill(ctx context.Context, paneID string) error
	// Send delivers a prompt and returns once the pane has started the turn.
	Send(ctx context.Context, paneID, text string) error
	Transcript(ctx context.Context, paneID string, limit int) ([]json.RawMessage, error)
	Screen(ctx context.Context, paneID string, lines int) (string, error)
}

type Pane struct {
	ID      string `json:"id"`
	Label   string `json:"taskLabel"`
	Dir     string `json:"cwd"`
	Command string `json:"command"`
	Role    string `json:"role"`
	Bridge  bool   `json:"bridge"`
}

// Title is how Carla refers to a pane out loud.
func (p Pane) Title() string {
	if p.Label != "" {
		return p.Label
	}
	if p.Command != "" {
		return p.Command
	}
	return p.ID
}

type Project struct {
	Name string `json:"name"`
	Path string `json:"path"`
}
