package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/ipc"
	"github.com/lemonsaurus/agency/internal/live"
	"github.com/lemonsaurus/agency/internal/session"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// liveBox gives the voice dispatcher the daemon's panes, projects and prompt bridge without SSH.
type liveBox struct {
	tc     *tmux.Client
	mgr    *session.Manager
	socket string
	home   string
}

func (b *liveBox) Panes(ctx context.Context) ([]live.Pane, error) {
	panes, err := b.tc.ListPanes(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	listed := make([]live.Pane, 0, len(panes))
	for _, pane := range panes {
		if seen[pane.ID] {
			continue
		}
		seen[pane.ID] = true
		bridge := bridgeListening(b.socket, pane.ID)
		if !bridge && pane.Command != "pi" {
			continue
		}
		listed = append(listed, live.Pane{ID: pane.ID, Label: pane.TaskLabel, Dir: pane.CWD, Command: pane.Command, Role: pane.Role, Bridge: bridge})
	}
	return listed, nil
}

func (b *liveBox) Projects() ([]live.Project, error) {
	found, err := projects(filepath.Join(b.home, "git"))
	if err != nil {
		return nil, err
	}
	listed := make([]live.Project, 0, len(found))
	for _, p := range found {
		listed = append(listed, live.Project{Name: p.Name, Path: p.Path})
	}
	return listed, nil
}

func (b *liveBox) Spawn(ctx context.Context, dir, label string) error {
	return b.mgr.SpawnAgentWindow(ctx, control.Requester{Human: true}, control.RoleController, "phone", "pi", dir, label)
}

func (b *liveBox) Kill(ctx context.Context, paneID string) error {
	return b.mgr.KillPane(ctx, paneID)
}

func (b *liveBox) Send(ctx context.Context, paneID, text string) error {
	path, err := ipc.BridgePath(b.socket, paneID)
	if err != nil {
		return err
	}
	id := make([]byte, 16)
	rand.Read(id)
	ctx, cancel := context.WithTimeout(ctx, ipc.MaxAskTimeout)
	defer cancel()
	reply, err := ipc.Ask(ctx, path, ipc.AskRequest{ID: hex.EncodeToString(id), Pane: paneID, Text: text, TimeoutMS: ipc.MaxAskTimeout.Milliseconds(), Detach: true})
	if err != nil {
		return err
	}
	if reply.Error != nil {
		return fmt.Errorf("%s", reply.Error.Message)
	}
	if !reply.Accepted {
		return fmt.Errorf("the session did not start the turn")
	}
	return nil
}

func (b *liveBox) Transcript(ctx context.Context, paneID string, limit int) ([]json.RawMessage, error) {
	path, err := ipc.BridgePath(b.socket, paneID)
	if err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	rand.Read(id)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	reply, err := ipc.Transcript(ctx, path, ipc.TranscriptRequest{ID: hex.EncodeToString(id), Limit: limit})
	if err != nil {
		return nil, err
	}
	if reply.Error != nil {
		return nil, fmt.Errorf("%s", reply.Error.Message)
	}
	return reply.Entries, nil
}

func (b *liveBox) Screen(ctx context.Context, paneID string, lines int) (string, error) {
	return b.tc.CapturePaneContent(ctx, paneID, lines)
}

func newLiveManager(tc *tmux.Client, mgr *session.Manager, socket string) *live.Manager {
	home, _ := os.UserHomeDir()
	key, err := voiceAPIKey(os.Getenv("OPENAI_API_KEY"), filepath.Join(home, ".pi", "agent", "private.env"))
	if err != nil {
		key = ""
	}
	agentsDir := filepath.Join(home, ".agents")
	return live.NewManager(&liveBox{tc: tc, mgr: mgr, socket: socket, home: home}, key,
		func() (string, error) { return persona(agentsDir) },
		filepath.Join(agentsDir, "voice"), filepath.Join(agentsDir, "run", "agency", "voice-memory.jsonl"))
}

// runLive relays one voice request from the phone to the daemon.
func runLive(sessionName string, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud live start <json>|status|said <text>|discord <json>|discord-done <id> [error]|close")
		os.Exit(1)
	}
	request := map[string]any{"op": args[0]}
	switch args[0] {
	case "start", "discord":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "Usage: agency cloud live "+args[0]+" <json>")
			os.Exit(1)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(args[1]), &body); err != nil {
			fmt.Fprintln(os.Stderr, "Error: invalid JSON")
			os.Exit(1)
		}
		for key, value := range body {
			request[key] = value
		}
	case "said":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "Usage: agency cloud live said <text>")
			os.Exit(1)
		}
		request["text"] = args[1]
	case "discord-done":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: agency cloud live discord-done <id> [error]")
			os.Exit(1)
		}
		var id int
		fmt.Sscanf(args[1], "%d", &id)
		request["id"] = id
		if len(args) > 2 {
			request["error"] = args[2]
		}
	case "status", "close":
	default:
		fmt.Fprintln(os.Stderr, "Unknown live op:", args[0])
		os.Exit(1)
	}
	payload, _ := json.Marshal(request)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	reply, err := ipc.SendMessageContext(ctx, socketPath(sessionName), "live:"+string(payload))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	if len(reply) >= 6 && reply[:6] == "error:" {
		fmt.Fprintln(os.Stderr, reply)
		os.Exit(1)
	}
	fmt.Println(reply)
}
