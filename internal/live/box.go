// Package live runs Carla's voice backend on the box: a sideband on the GPT-Live session, a
// dispatcher that queues work for Pi panes, and narration of their progress back into the call.
package live

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"time"
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
	// Model is the provider/model the pane's Pi runs now, and its thinking level, from its bridge.
	Model(ctx context.Context, paneID string) (model, thinking string, err error)
}

// ModelInfo is a session's model in status JSON. Model is null when the bridge could not say,
// with the reason in ModelUnavailable.
type ModelInfo struct {
	Model            *string `json:"model"`
	Thinking         string  `json:"thinking,omitempty"`
	ModelUnavailable string  `json:"model_unavailable,omitempty"`
}

func unavailableModel(reason string) ModelInfo {
	return ModelInfo{ModelUnavailable: reason}
}

// modelOf asks a pane's bridge for its model. A pane without a bridge is still starting.
func modelOf(ctx context.Context, box Box, pane Pane) ModelInfo {
	if !pane.Bridge {
		return unavailableModel("the session is still starting")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	model, thinking, err := box.Model(ctx, pane.ID)
	if err != nil {
		return unavailableModel("the session's bridge did not say: " + err.Error())
	}
	return ModelInfo{Model: &model, Thinking: thinking}
}

// modelsOf asks every pane's bridge at once.
func modelsOf(ctx context.Context, box Box, panes []Pane) []ModelInfo {
	models := make([]ModelInfo, len(panes))
	var wait sync.WaitGroup
	for i, pane := range panes {
		wait.Add(1)
		go func() {
			defer wait.Done()
			models[i] = modelOf(ctx, box, pane)
		}()
	}
	wait.Wait()
	return models
}

func (m ModelInfo) into(fields map[string]any) {
	fields["model"] = m.Model
	if m.Thinking != "" {
		fields["thinking"] = m.Thinking
	}
	if m.ModelUnavailable != "" {
		fields["model_unavailable"] = m.ModelUnavailable
	}
}

type Pane struct {
	ID      string `json:"id"`
	Label   string `json:"taskLabel"`
	Dir     string `json:"cwd"`
	Command string `json:"command"`
	Role    string `json:"role"`
	Bridge  bool   `json:"bridge"`

	before map[string]bool // panes that existed before this one was spawned
}

// Title is how Carla refers to a pane out loud. Pi names a fresh pane from its first prompt;
// until then the project stands in.
func (p Pane) Title() string {
	if p.Label != "" {
		return p.Label
	}
	if p.Dir != "" {
		return filepath.Base(p.Dir)
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
