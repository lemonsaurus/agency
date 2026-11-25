package ipc

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lemonsaurus/agency/internal/budget"
	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/order"
)

// Handler processes IPC commands from the socket.
type Handler interface {
	Capabilities() control.Capabilities
	ResolveRequester(ctx context.Context, pid int) (control.Requester, error)
	SpawnAgent(ctx context.Context, requester control.Requester, role control.Role, name, dir, label, prompt string) (string, error)
	SpawnAgentWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, name, dir, label, prompt string) (string, error)
	SpawnCommand(ctx context.Context, requester control.Requester, role control.Role, command, dir, label string) (string, error)
	SpawnCommandWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, command, dir, label string) (string, error)
	TaskLabel(ctx context.Context, requester control.Requester, paneID string, label *string) (string, error)
	InitTaskLabel(ctx context.Context, requester control.Requester, label string) (string, error)
	KillPane(ctx context.Context, requester control.Requester, paneID string) error
	KillWindow(ctx context.Context, requester control.Requester, windowName string) error
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
	// Visibility tells the cloud host which viewers are on screen.
	Visibility(ctx context.Context) error
	// Focus tells the cloud host this device's terminal took focus.
	Focus(ctx context.Context) error
	// Live serves the phone's voice requests: session start, status, Discord relay.
	Live(ctx context.Context, payload string) (string, error)
	// Cloud serves the sky screen and the viewers' wait for the cloud link.
	Cloud(ctx context.Context, payload string) (string, error)
	// WindowOrder applies a device's moves to the sky box's window order and
	// returns the order.
	WindowOrder(ctx context.Context, moves []order.Move) ([]string, error)
	// WindowMoved tells the sky box about a sky window moved on this device.
	WindowMoved(ctx context.Context) error
}

type spawnPayload struct {
	Window  string `json:"window,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Command string `json:"command,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Role    string `json:"role,omitempty"`
	Label   string `json:"label,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
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
	Again bool   `json:"again,omitempty"` // deliver even an identical message sent within ResendWindow
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
	sends    sendLedger
	slots    chan struct{}

	// Budget answers budget and job requests; nil leaves them unknown.
	Budget    *budget.Budget
	Admission *Admission
}

// NewServer creates a socket server at the given path.
func NewServer(path string, handler Handler) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		path:    path,
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
		slots:   make(chan struct{}, 32),
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
		select {
		case s.slots <- struct{}{}:
		case <-s.ctx.Done():
			conn.Close()
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			release := sync.OnceFunc(func() { <-s.slots })
			defer release()
			s.handleConn(conn, release)
		}()
	}
}

var requestReadTimeout = 5 * time.Second

// replyWriter bounds a reply even when the caller stops reading it.
type replyWriter struct{ net.Conn }

func (w replyWriter) Write(data []byte) (int, error) {
	if err := w.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return 0, err
	}
	return w.Conn.Write(data)
}

// handleConn serves one request. release frees the connection's admission
// slot early, for a wait that holds the connection but no other resource.
func (s *Server) handleConn(conn net.Conn, release func()) {
	defer conn.Close()
	stop := context.AfterFunc(s.ctx, func() { conn.Close() })
	defer stop()
	conn.SetReadDeadline(time.Now().Add(requestReadTimeout))
	writer := replyWriter{conn}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), MaxAskBytes)
	if !scanner.Scan() {
		return
	}
	conn.SetReadDeadline(time.Time{})
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
		if (err != nil || !requester.Human) && !reminder(strings.TrimPrefix(line, "live:")) {
			fmt.Fprintln(writer, "error: voice requests come from Lemon only")
			return
		}
		reply, err := s.handler.Live(s.ctx, strings.TrimPrefix(line, "live:"))
		if err != nil {
			fmt.Fprintf(writer, "error: %v\n", err)
			return
		}
		fmt.Fprintln(writer, strings.ReplaceAll(reply, "\n", " "))
		return
	}
	if payload, ok := strings.CutPrefix(line, "cloud:"); ok {
		// A viewer may wait here for minutes; its hangup ends the wait, and
		// waiting viewers take no admission slot.
		release()
		ctx, cancel := context.WithCancel(s.ctx)
		defer cancel()
		go func() {
			_, _ = conn.Read(make([]byte, 1))
			cancel()
		}()
		reply, err := s.handler.Cloud(ctx, payload)
		if err != nil {
			fmt.Fprintf(writer, "error: %v\n", err)
			return
		}
		fmt.Fprintln(writer, strings.ReplaceAll(reply, "\n", " "))
		return
	}
	response, err := s.dispatch(line, peerPIDForConn(conn))
	if err != nil {
		log.Printf("ipc: dispatch %q: %v", line, err)
		fmt.Fprintf(writer, "error: %v\n", err)
		return
	}
	if response == "" {
		response = "ok"
	}
	fmt.Fprintln(writer, response)
}

// bridgeSendTimeout bounds the wait for a busy Pi to confirm a send.
var bridgeSendTimeout = 10 * time.Second

// deliver hands an agent's message to the target's Pi bridge, which queues it
// without touching the editor. It is false only when Pi never took the text,
// so typing it instead cannot deliver it twice.
func (s *Server) deliver(paneID, text string) bool {
	path, err := BridgePath(s.path, paneID)
	if err != nil {
		return false
	}
	id := make([]byte, 16)
	rand.Read(id)
	ctx, cancel := context.WithTimeout(s.ctx, bridgeSendTimeout)
	defer cancel()
	err = Send(ctx, path, SendRequest{ID: hex.EncodeToString(id), Text: text})
	var refusal *BridgeRefusal
	if errors.Is(err, ErrNoBridge) || errors.As(err, &refusal) {
		return false
	}
	if err != nil {
		log.Printf("ipc: send to %s: the Pi bridge took the message without confirming: %v", paneID, err)
	}
	return true
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
	if s.Budget != nil {
		if reply, ok, err := s.budgetDispatch(line, pid); ok {
			return reply, err
		}
	}
	if line == "capabilities" {
		capabilities := s.handler.Capabilities()
		capabilities.PromptBridge = true
		data, _ := json.Marshal(capabilities)
		return string(data), nil
	}
	if line == "relayout" {
		return "", s.handler.Relayout(s.ctx)
	}
	if line == "visibility" {
		return "", s.handler.Visibility(s.ctx)
	}
	if line == "focus" {
		return "", s.handler.Focus(s.ctx)
	}
	if line == "sync-cloud" {
		return s.handler.SyncCloud(s.ctx)
	}
	if line == "window-moved" {
		return "", s.handler.WindowMoved(s.ctx)
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
			return s.handler.SpawnCommand(s.ctx, requester, role, command, dir, "")
		}
		name, dir := splitDirSuffix(arg)
		return s.handler.SpawnAgent(s.ctx, requester, role, name, dir, "", "")
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
			if payload.Prompt != "" {
				return "", fmt.Errorf("prompt needs an agent, not a command")
			}
			if payload.Window != "" {
				return s.handler.SpawnCommandWindow(s.ctx, requester, role, payload.Window, payload.Command, payload.Dir, payload.Label)
			}
			return s.handler.SpawnCommand(s.ctx, requester, role, payload.Command, payload.Dir, payload.Label)
		}
		if payload.Agent == "" {
			return "", fmt.Errorf("agent or command is required")
		}
		if payload.Window != "" {
			return s.handler.SpawnAgentWindow(s.ctx, requester, role, payload.Window, payload.Agent, payload.Dir, payload.Label, payload.Prompt)
		}
		return s.handler.SpawnAgent(s.ctx, requester, role, payload.Agent, payload.Dir, payload.Label, payload.Prompt)
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
		return "", s.handler.KillPane(s.ctx, requester, arg)
	case "kill-window":
		requester, err := s.requester(pid)
		if err != nil {
			return "", err
		}
		if !requester.CanKillWindow() {
			return "", fmt.Errorf("worker panes cannot kill windows")
		}
		return "", s.handler.KillWindow(s.ctx, requester, arg)
	case "rename-window":
		var payload renameWindowPayload
		if err := json.Unmarshal([]byte(arg), &payload); err != nil {
			return "", fmt.Errorf("invalid rename-window payload: %w", err)
		}
		if payload.Target == "" || payload.Name == "" {
			return "", fmt.Errorf("target and name are required")
		}
		return "", s.handler.RenameWindow(s.ctx, payload.Target, payload.Name)
	case "window-order":
		var moves []order.Move
		if err := json.Unmarshal([]byte(arg), &moves); err != nil {
			return "", fmt.Errorf("invalid window-order payload: %w", err)
		}
		names, err := s.handler.WindowOrder(s.ctx, moves)
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(names)
		return string(data), nil
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
		send := func() error {
			if !requester.Human && payload.Enter && s.deliver(payload.Pane, requester.From()+" "+payload.Text) {
				return nil
			}
			return s.handler.SendText(s.ctx, requester, payload.Pane, payload.Text, payload.Enter)
		}
		if requester.Human || payload.Again {
			return "", send()
		}
		at, err := s.sends.deliver(s.ctx, sendKey(requester.PaneID, payload.Pane, payload.Text, payload.Enter), send)
		if err != nil || at.IsZero() {
			return "", err
		}
		return AlreadySent + at.UTC().Format(time.RFC3339), nil
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

// reminder is whether a live request only files a reminder, which agents may do.
func reminder(payload string) bool {
	var request struct {
		Op string `json:"op"`
	}
	return json.Unmarshal([]byte(payload), &request) == nil && request.Op == "remind"
}
