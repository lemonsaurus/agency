package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/config"
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
	return b.mgr.KillPane(ctx, control.Requester{Role: control.RoleController, Human: true}, paneID)
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
	key, err := privateKey("OPENAI_API_KEY", os.Getenv("OPENAI_API_KEY"), filepath.Join(home, ".pi", "agent", "private.env"))
	if err != nil {
		key = ""
	}
	agentsDir := filepath.Join(home, ".agents")
	manager := live.NewManager(&liveBox{tc: tc, mgr: mgr, socket: socket, home: home}, key,
		func() (string, error) { return persona(agentsDir) },
		filepath.Join(agentsDir, "voice"), filepath.Join(agentsDir, "run", "agency", "voice-memory.jsonl"))
	if pusher, err := live.NewPusher(filepath.Join(home, ".config", "agency", "fcm-service-account.json")); err == nil {
		manager.SetPush(pusher)
	} else if !os.IsNotExist(err) {
		log.Printf("reminders: push disabled: %v", err)
	}
	return manager
}

// remindersPath is the store the live manager writes, next to the voice memory.
func remindersPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agents", "run", "agency", "reminders.json")
}

// runRemind files a one-off reminder on the sky host: directly on the host, forwarded over SSH on earth.
func runRemind(args []string) {
	cfg := loadConfig()
	if cfg.Cloud.Host == "" {
		runLive(cfg.Session.Name, append([]string{"remind"}, args...))
		return
	}
	out, err := (&cloud.Client{Host: cfg.Cloud.Host}).Run(context.Background(), 40*time.Second, append([]string{"remind"}, args...)...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	fmt.Print(out)
}

// runActivity prints the voice backend's activity tool result, for Pi and anyone else on the box.
func runActivity(cfg *config.Config) {
	home, _ := os.UserHomeDir()
	box := &liveBox{tc: tmux.NewClient(cfg.Session.Name, ""), socket: socketPath(cfg.Session.Name), home: home}
	sessions, err := live.Activity(context.Background(), box, filepath.Join(home, ".agents", "run", "agency", "activity.jsonl"), time.Now(), 12*time.Hour)
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(sessions)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// runLive relays one voice request from the phone to the daemon.
func runLive(sessionName string, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud live start <json>|status|said <text>|discord <json>|discord-done <id> [error]|reminder-done <id> [error]|reminders|push-token <token> <zone>|remind [<when> <text>...]|close")
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
	case "discord-done", "reminder-done":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: agency cloud live "+args[0]+" <id> [error]")
			os.Exit(1)
		}
		var id int
		fmt.Sscanf(args[1], "%d", &id)
		request["id"] = id
		if len(args) > 2 {
			request["error"] = args[2]
		}
	case "push-token":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "Usage: agency cloud live push-token <token> <zone>")
			os.Exit(1)
		}
		request["token"], request["zone"] = args[1], args[2]
	case "remind":
		if len(args) == 2 {
			fmt.Fprintln(os.Stderr, "Usage: agency remind <+20m|YYYY-MM-DDTHH:MM> <text>")
			os.Exit(1)
		}
		if len(args) > 2 {
			request["when"], request["text"] = args[1], strings.Join(args[2:], " ")
		}
	case "status", "close", "reminders":
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
