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
	// Model is what the pane's Pi bridge says about its model.
	Model(ctx context.Context, paneID string) (PaneModel, error)
}

// PaneModel is a pane's selected provider/model, which may be a router like jev/Auto, the
// provider/model that wrote its latest answer (nil from bridges that predate it, empty before the
// first answer), and its thinking level.
type PaneModel struct {
	Selected string
	Answered *string
	Thinking string
}

// ModelInfo is a session's model in status JSON: Model is the one that actually answered last, null
// with the reason in ModelUnavailable when the bridge cannot say.
type ModelInfo struct {
	Model            *string `json:"model"`
	SelectedModel    string  `json:"selected_model,omitempty"`
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
	model, err := box.Model(ctx, pane.ID)
	if err != nil {
		return unavailableModel("the session's bridge did not say: " + err.Error())
	}
	info := ModelInfo{SelectedModel: model.Selected, Thinking: model.Thinking}
	switch {
	case model.Answered == nil:
		info.ModelUnavailable = "the session's Pi predates reporting the model that answered; /reload it"
	case *model.Answered == "":
		info.ModelUnavailable = "the session has not answered yet"
	default:
		info.Model = model.Answered
	}
	return info
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
	if m.SelectedModel != "" {
		fields["selected_model"] = m.SelectedModel
	}
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
