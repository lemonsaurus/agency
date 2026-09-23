package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/lemonsaurus/agency/internal/control"
)

// Handler processes IPC commands from the socket.
type Handler interface {
	Capabilities() control.Capabilities
	ResolveRequester(ctx context.Context, pid int) (control.Requester, error)
	SpawnAgent(ctx context.Context, requester control.Requester, role control.Role, name, dir, label string) error
	SpawnAgentWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, name, dir, label string) error
	SpawnCommand(ctx context.Context, requester control.Requester, role control.Role, command, dir, label string) error
	SpawnCommandWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, command, dir, label string) error
	TaskLabel(ctx context.Context, requester control.Requester, paneID string, label *string) (string, error)
	InitTaskLabel(ctx context.Context, requester control.Requester, label string) (string, error)
	KillPane(ctx context.Context, paneID string) error
	KillWindow(ctx context.Context, windowName string) error
	RenameWindow(ctx context.Context, target, name string) error
	MoveWindow(ctx context.Context, target string, index int) error
	MovePane(ctx context.Context, paneID, windowName string) error
	SendText(ctx context.Context, requester control.Requester, paneID, text string, enter bool) error
	SetLayout(ctx context.Context, layout string) error
	Relayout(ctx context.Context) error
	BroadcastKeys(ctx context.Context, keys string) error
	ReplacePane(ctx context.Context, requester control.Requester, command, dir string) (string, error)
	RequestPromotion(ctx context.Context, requester control.Requester, reason string) error
	ApprovePromotion(ctx context.Context, requester control.Requester, paneID string) error
	SyncCloud(ctx context.Context) (string, error)
	// Live serves the phone's voice requests: session start, status, Discord relay.
	Live(ctx context.Context, payload string) (string, error)
}

type spawnPayload struct {
	Window  string `json:"window,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Command string `json:"command,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Role    string `json:"role,omitempty"`
	Label   string `json:"label,omitempty"`
}

type labelPayload struct {
	Pane  string  `json:"pane,omitempty"`
	Label *string `json:"label,omitempty"`
}

type renameWindowPayload struct {
	Target string `json:"target"`
	Name   string `json:"name"`
}

type moveWindowPayload struct {
	Target string `json:"target"`
	Index  int    `json:"index"`
}

type sendPayload struct {
	Pane  string `json:"pane"`
	Text  string `json:"text"`
	Enter bool   `json:"enter"`
}

type movePayload struct {
	Pane   string `json:"pane"`
	Window string `json:"window"`
}

type replacementPayload struct {
	Command string `json:"command,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

type promotionPayload struct {
	Reason string `json:"reason"`
}

// Server listens on a unix socket for agent spawn/control requests.
type Server struct {
	path     string
	listener net.Listener
	handler  Handler
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewServer creates a socket server at the given path.
func NewServer(path string, handler Handler) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		path:    path,
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Path returns the socket file path.
func (s *Server) Path() string {
	return s.path
}

// Start begins listening on the socket. Call Close() to stop.
func (s *Server) Start() error {
	_ = os.Remove(s.path)

	listener, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.path, err)
	}
	s.listener = listener

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.acceptLoop()
	}()
	return nil
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return
			default:
				log.Printf("ipc: accept error: %v", err)
				return
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), MaxAskBytes)
	if !scanner.Scan() {
		return
	}
	line := strings.TrimSpace(scanner.Text())
	if line == "" {
		return
	}
	if strings.HasPrefix(line, "ask:") {
		s.handleAsk(conn, conn, strings.TrimPrefix(line, "ask:"))
		return
	}
	if strings.HasPrefix(line, "live:") {
		requester, err := s.requester(peerPIDForConn(conn))
		if err != nil || !requester.Human {
			fmt.Fprintln(conn, "error: voice requests come from Lemon only")
			return
		}
		reply, err := s.handler.Live(s.ctx, strings.TrimPrefix(line, "live:"))
		if err != nil {
			fmt.Fprintf(conn, "error: %v\n", err)
			return
		}
		fmt.Fprintln(conn, strings.ReplaceAll(reply, "\n", " "))
		return
	}
	response, err := s.dispatch(line, peerPIDForConn(conn))
	if err != nil {
		log.Printf("ipc: dispatch %q: %v", line, err)
		fmt.Fprintf(conn, "error: %v\n", err)
		return
	}
	if response == "" {
		response = "ok"
	}
	fmt.Fprintln(conn, response)
}

func (s *Server) requester(pid int) (control.Requester, error) {
	if pid <= 0 {
		return control.Requester{}, fmt.Errorf("cannot identify agency requester")
	}
	return s.handler.ResolveRequester(s.ctx, pid)
}

func requestedRole(requester control.Requester, value string) (control.Role, error) {
	if value == "" {
		if requester.Human {
			return control.RoleController, nil
		}
		return "", fmt.Errorf("programmatic spawns require an explicit role")
	}
	role, err := control.ParseRole(value)
	if err != nil {
		return "", err
	}
	if requester.Human {
		if role == control.RoleController {
			return role, nil
		}
		if requester.PaneID == "" {
			return "", fmt.Errorf("create a controller before requesting a child role")
		}
	}
	return requester.ChildRole(role)
}

func (s *Server) dispatch(line string, pid int) (string, error) {
	if line == "capabilities" {
		capabilities := s.handler.Capabilities()
		capabilities.PromptBridge = true
		data, _ := json.Marshal(capabilities)
		return string(data), nil
	}
	if line == "relayout" {
		return "", s.handler.Relayout(s.ctx)
	}
	if line == "sync-cloud" {
		return s.handler.SyncCloud(s.ctx)
	}
	if line == "whoami" {
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(requester)
		return string(data), nil
	}
	if line == "replace" {
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		return s.handler.ReplacePane(s.ctx, requester, "", "")
	}

	parts := strings.SplitN(line, ":", 2)
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid message: %q", line)
	}

	cmd := parts[0]
	arg := parts[1]

	switch cmd {
	case "spawn":
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		role, err := requestedRole(requester, "")
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(arg, "cmd:") {
			command, dir := splitDirSuffix(strings.TrimPrefix(arg, "cmd:"))
			return "", s.handler.SpawnCommand(s.ctx, requester, role, command, dir, "")
		}
		name, dir := splitDirSuffix(arg)
		return "", s.handler.SpawnAgent(s.ctx, requester, role, name, dir, "")
	case "spawn-role", "spawn-window":
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		var payload spawnPayload
		if err := json.Unmarshal([]byte(arg), &payload); err != nil {
			return "", fmt.Errorf("invalid %s payload: %w", cmd, err)
		}
		if cmd == "spawn-window" && payload.Window == "" {
			return "", fmt.Errorf("window name is required")
		}
		role, err := requestedRole(requester, payload.Role)
		if err != nil {
			return "", err
		}
		if err := control.ValidateTaskLabel(payload.Label, !requester.Human); err != nil {
			return "", err
		}
		if payload.Command != "" {
			if payload.Window != "" {
				return "", s.handler.SpawnCommandWindow(s.ctx, requester, role, payload.Window, payload.Command, payload.Dir, payload.Label)
			}
			return "", s.handler.SpawnCommand(s.ctx, requester, role, payload.Command, payload.Dir, payload.Label)
		}
		if payload.Agent == "" {
			return "", fmt.Errorf("agent or command is required")
		}
		if payload.Window != "" {
			return "", s.handler.SpawnAgentWindow(s.ctx, requester, role, payload.Window, payload.Agent, payload.Dir, payload.Label)
		}
		return "", s.handler.SpawnAgent(s.ctx, requester, role, payload.Agent, payload.Dir, payload.Label)
	case "label", "label-if-empty":
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		var payload labelPayload
		if err := json.Unmarshal([]byte(arg), &payload); err != nil {
			return "", fmt.Errorf("invalid label payload: %w", err)
		}
		var label string
		if cmd == "label-if-empty" {
			if payload.Pane != "" || payload.Label == nil {
				return "", fmt.Errorf("label-if-empty requires a label for the current pane")
			}
			label, err = s.handler.InitTaskLabel(s.ctx, requester, *payload.Label)
		} else {
			label, err = s.handler.TaskLabel(s.ctx, requester, payload.Pane, payload.Label)
		}
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(label)
		return string(data), nil
	case "kill":
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		if !requester.CanKillPane(arg) {
			return "", fmt.Errorf("worker panes may only kill their own pane")
		}
		return "", s.handler.KillPane(s.ctx, arg)
	case "kill-window":
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		if !requester.CanKillWindow() {
			return "", fmt.Errorf("worker panes cannot kill windows")
		}
		return "", s.handler.KillWindow(s.ctx, arg)
	case "rename-window":
		var payload renameWindowPayload
		if err := json.Unmarshal([]byte(arg), &payload); err != nil {
			return "", fmt.Errorf("invalid rename-window payload: %w", err)
		}
		if payload.Target == "" || payload.Name == "" {
			return "", fmt.Errorf("target and name are required")
		}
		return "", s.handler.RenameWindow(s.ctx, payload.Target, payload.Name)
	case "move-window":
		var payload moveWindowPayload
		if err := json.Unmarshal([]byte(arg), &payload); err != nil {
			return "", fmt.Errorf("invalid move-window payload: %w", err)
		}
		if payload.Target == "" || payload.Index < 0 {
			return "", fmt.Errorf("target and index are required")
		}
		return "", s.handler.MoveWindow(s.ctx, payload.Target, payload.Index)
	case "send":
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		var payload sendPayload
		if err := json.Unmarshal([]byte(arg), &payload); err != nil {
			return "", fmt.Errorf("invalid send payload: %w", err)
		}
		if payload.Pane == "" {
			return "", fmt.Errorf("pane is required")
		}
		return "", s.handler.SendText(s.ctx, requester, payload.Pane, payload.Text, payload.Enter)
	case "move":
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		var payload movePayload
		if err := json.Unmarshal([]byte(arg), &payload); err != nil {
			return "", fmt.Errorf("invalid move payload: %w", err)
		}
		if payload.Pane == "" || payload.Window == "" {
			return "", fmt.Errorf("pane and window are required")
		}
		if !requester.CanMovePane(payload.Pane) {
			return "", fmt.Errorf("worker panes may only move their own pane")
		}
		return "", s.handler.MovePane(s.ctx, payload.Pane, payload.Window)
	case "layout":
		return "", s.handler.SetLayout(s.ctx, arg)
	case "broadcast-keys":
		return "", s.handler.BroadcastKeys(s.ctx, arg)
	case "replace":
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		var payload replacementPayload
		if err := json.Unmarshal([]byte(arg), &payload); err != nil {
			return "", fmt.Errorf("invalid replacement payload: %w", err)
		}
		return s.handler.ReplacePane(s.ctx, requester, payload.Command, payload.Dir)
	case "promotion-request":
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		var payload promotionPayload
		if err := json.Unmarshal([]byte(arg), &payload); err != nil {
			return "", fmt.Errorf("invalid promotion-request payload: %w", err)
		}
		return "", s.handler.RequestPromotion(s.ctx, requester, strings.TrimSpace(payload.Reason))
	case "promotion-approve":
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		if arg == "" {
			return "", fmt.Errorf("pane is required")
		}
		return "", s.handler.ApprovePromotion(s.ctx, requester, arg)
	default:
		return "", fmt.Errorf("unknown command: %q", cmd)
	}
}

// splitDirSuffix splits a string on "@/" to extract an optional absolute
// directory path suffix. For example "claude@/home/user" returns ("claude", "/home/user").
func splitDirSuffix(s string) (value, dir string) {
	idx := strings.Index(s, "@/")
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}

// Close stops the server, removes the socket file, and waits for goroutines.
func (s *Server) Close() error {
	s.cancel()
	if s.listener != nil {
		s.listener.Close()
	}
	s.wg.Wait()
	_ = os.Remove(s.path)
	return nil
}

// SendMessage is a client helper that sends a one-line message to a socket.
func SendMessage(socketPath, message string) (string, error) {
	return SendMessageContext(context.Background(), socketPath, message)
}

func SendMessageContext(ctx context.Context, socketPath, message string) (string, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return "", fmt.Errorf("connecting to %s: %w", socketPath, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	fmt.Fprintf(conn, "%s\n", message)

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	if scanner.Scan() {
		return scanner.Text(), nil
	}
	return "", ctx.Err()
}
