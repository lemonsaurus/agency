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
	ResolveRequester(ctx context.Context, pid int) (control.Requester, error)
	SpawnAgent(ctx context.Context, requester control.Requester, role control.Role, name, dir string) error
	SpawnAgentWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, name, dir string) error
	SpawnCommand(ctx context.Context, requester control.Requester, role control.Role, command, dir string) error
	SpawnCommandWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, command, dir string) error
	KillPane(ctx context.Context, paneID string) error
	KillWindow(ctx context.Context, windowName string) error
	RenameWindow(ctx context.Context, target, name string) error
	MovePane(ctx context.Context, paneID, windowName string) error
	SendText(ctx context.Context, requester control.Requester, paneID, text string, enter bool) error
	SetLayout(ctx context.Context, layout string) error
	Relayout(ctx context.Context) error
	BroadcastKeys(ctx context.Context, keys string) error
}

type spawnPayload struct {
	Window  string `json:"window,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Command string `json:"command,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Role    string `json:"role,omitempty"`
}

type renameWindowPayload struct {
	Target string `json:"target"`
	Name   string `json:"name"`
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
	if !scanner.Scan() {
		return
	}
	line := strings.TrimSpace(scanner.Text())
	if line == "" {
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
	var role control.Role
	var err error
	if value != "" {
		role, err = control.ParseRole(value)
		if err != nil {
			return "", err
		}
	}
	return requester.ChildRole(role)
}

func (s *Server) dispatch(line string, pid int) (string, error) {
	if line == "relayout" {
		return "", s.handler.Relayout(s.ctx)
	}
	if line == "whoami" {
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(requester)
		return string(data), nil
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
			return "", s.handler.SpawnCommand(s.ctx, requester, role, command, dir)
		}
		name, dir := splitDirSuffix(arg)
		return "", s.handler.SpawnAgent(s.ctx, requester, role, name, dir)
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
		if payload.Command != "" {
			if payload.Window != "" {
				return "", s.handler.SpawnCommandWindow(s.ctx, requester, role, payload.Window, payload.Command, payload.Dir)
			}
			return "", s.handler.SpawnCommand(s.ctx, requester, role, payload.Command, payload.Dir)
		}
		if payload.Agent == "" {
			return "", fmt.Errorf("agent or command is required")
		}
		if payload.Window != "" {
			return "", s.handler.SpawnAgentWindow(s.ctx, requester, role, payload.Window, payload.Agent, payload.Dir)
		}
		return "", s.handler.SpawnAgent(s.ctx, requester, role, payload.Agent, payload.Dir)
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
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return "", fmt.Errorf("connecting to %s: %w", socketPath, err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "%s\n", message)

	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		return scanner.Text(), nil
	}
	return "", nil
}
