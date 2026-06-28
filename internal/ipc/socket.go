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
)

// Handler processes IPC commands from the socket.
type Handler interface {
	SpawnAgent(ctx context.Context, name, dir string) error
	SpawnAgentWindow(ctx context.Context, windowName, name, dir string) error
	SpawnCommand(ctx context.Context, command, dir string) error
	SpawnCommandWindow(ctx context.Context, windowName, command, dir string) error
	KillPane(ctx context.Context, paneID string) error
	KillWindow(ctx context.Context, windowName string) error
	RenameWindow(ctx context.Context, target, name string) error
	SetLayout(ctx context.Context, layout string) error
	Relayout(ctx context.Context) error
	BroadcastKeys(ctx context.Context, keys string) error
}

type spawnWindowPayload struct {
	Window  string `json:"window"`
	Agent   string `json:"agent,omitempty"`
	Command string `json:"command,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

type renameWindowPayload struct {
	Target string `json:"target"`
	Name   string `json:"name"`
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
	// Remove stale socket file if present.
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
	if err := s.dispatch(line); err != nil {
		log.Printf("ipc: dispatch %q: %v", line, err)
		fmt.Fprintf(conn, "error: %v\n", err)
		return
	}
	fmt.Fprintf(conn, "ok\n")
}

func (s *Server) dispatch(line string) error {
	// Handle commands without arguments.
	if line == "relayout" {
		return s.handler.Relayout(s.ctx)
	}

	parts := strings.SplitN(line, ":", 2)
	if len(parts) < 2 {
		return fmt.Errorf("invalid message: %q", line)
	}

	cmd := parts[0]
	arg := parts[1]

	switch cmd {
	case "spawn":
		if strings.HasPrefix(arg, "cmd:") {
			command, dir := splitDirSuffix(strings.TrimPrefix(arg, "cmd:"))
			return s.handler.SpawnCommand(s.ctx, command, dir)
		}
		name, dir := splitDirSuffix(arg)
		return s.handler.SpawnAgent(s.ctx, name, dir)
	case "spawn-window":
		var payload spawnWindowPayload
		if err := json.Unmarshal([]byte(arg), &payload); err != nil {
			return fmt.Errorf("invalid spawn-window payload: %w", err)
		}
		if payload.Window == "" {
			return fmt.Errorf("window name is required")
		}
		if payload.Command != "" {
			return s.handler.SpawnCommandWindow(s.ctx, payload.Window, payload.Command, payload.Dir)
		}
		if payload.Agent == "" {
			return fmt.Errorf("agent or command is required")
		}
		return s.handler.SpawnAgentWindow(s.ctx, payload.Window, payload.Agent, payload.Dir)
	case "kill":
		return s.handler.KillPane(s.ctx, arg)
	case "kill-window":
		return s.handler.KillWindow(s.ctx, arg)
	case "rename-window":
		var payload renameWindowPayload
		if err := json.Unmarshal([]byte(arg), &payload); err != nil {
			return fmt.Errorf("invalid rename-window payload: %w", err)
		}
		if payload.Target == "" || payload.Name == "" {
			return fmt.Errorf("target and name are required")
		}
		return s.handler.RenameWindow(s.ctx, payload.Target, payload.Name)
	case "layout":
		return s.handler.SetLayout(s.ctx, arg)
	case "broadcast-keys":
		return s.handler.BroadcastKeys(s.ctx, arg)
	default:
		return fmt.Errorf("unknown command: %q", cmd)
	}
}

// splitDirSuffix splits a string on "@/" to extract an optional absolute
// directory path suffix. For example "claude@/home/user" returns ("claude", "/home/user").
// If no "@/" is found, dir is empty.
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
