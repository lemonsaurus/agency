package ipc

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lemonsaurus/agency/internal/control"
)

type spawnRecord struct {
	window string
	name   string
	dir    string
	role   control.Role
}

type commandRecord struct {
	window  string
	command string
	dir     string
	role    control.Role
}

type renameRecord struct {
	target string
	name   string
}

type moveRecord struct {
	pane   string
	window string
}

type mockHandler struct {
	mu            sync.Mutex
	spawns        []spawnRecord
	commands      []commandRecord
	kills         []string
	windowKills   []string
	renames       []renameRecord
	moves         []moveRecord
	layouts       []string
	relayouts     int
	broadcastKeys []string
	failNext      bool
	requester     control.Requester
}

func (m *mockHandler) ResolveRequester(_ context.Context, _ int) (control.Requester, error) {
	if m.requester.Role != "" {
		return m.requester, nil
	}
	return control.Requester{PaneID: "%0", Role: control.RoleController, RootID: "%0"}, nil
}

func (m *mockHandler) SpawnAgent(_ context.Context, _ control.Requester, role control.Role, name, dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext {
		m.failNext = false
		return fmt.Errorf("spawn failed")
	}
	m.spawns = append(m.spawns, spawnRecord{name: name, dir: dir, role: role})
	return nil
}

func (m *mockHandler) SpawnAgentWindow(_ context.Context, _ control.Requester, role control.Role, windowName, name, dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spawns = append(m.spawns, spawnRecord{window: windowName, name: name, dir: dir, role: role})
	return nil
}

func (m *mockHandler) SpawnCommand(_ context.Context, _ control.Requester, role control.Role, command, dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands = append(m.commands, commandRecord{command: command, dir: dir, role: role})
	return nil
}

func (m *mockHandler) SpawnCommandWindow(_ context.Context, _ control.Requester, role control.Role, windowName, command, dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands = append(m.commands, commandRecord{window: windowName, command: command, dir: dir, role: role})
	return nil
}

func (m *mockHandler) KillPane(_ context.Context, paneID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kills = append(m.kills, paneID)
	return nil
}

func (m *mockHandler) KillWindow(_ context.Context, windowName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.windowKills = append(m.windowKills, windowName)
	return nil
}

func (m *mockHandler) RenameWindow(_ context.Context, target, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.renames = append(m.renames, renameRecord{target: target, name: name})
	return nil
}

func (m *mockHandler) MovePane(_ context.Context, paneID, windowName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.moves = append(m.moves, moveRecord{pane: paneID, window: windowName})
	return nil
}

func (m *mockHandler) SetLayout(_ context.Context, layout string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.layouts = append(m.layouts, layout)
	return nil
}

func (m *mockHandler) Relayout(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.relayouts++
	return nil
}

func (m *mockHandler) BroadcastKeys(_ context.Context, keys string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.broadcastKeys = append(m.broadcastKeys, keys)
	return nil
}

func TestServerSpawnAgent(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	resp, err := SendMessage(sockPath, "spawn:claude")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %q", resp)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.spawns) != 1 || h.spawns[0].name != "claude" || h.spawns[0].dir != "" {
		t.Errorf("expected spawn of 'claude' with no dir, got %v", h.spawns)
	}
}

func TestServerSpawnAgentWithDir(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	resp, err := SendMessage(sockPath, "spawn:claude@/home/user/project")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %q", resp)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.spawns) != 1 || h.spawns[0].name != "claude" || h.spawns[0].dir != "/home/user/project" {
		t.Errorf("expected spawn of 'claude' in '/home/user/project', got %v", h.spawns)
	}
}

func TestServerSpawnCommand(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	resp, err := SendMessage(sockPath, "spawn:cmd:aider --yes")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %q", resp)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.commands) != 1 || h.commands[0].command != "aider --yes" || h.commands[0].dir != "" {
		t.Errorf("expected command 'aider --yes' with no dir, got %v", h.commands)
	}
}

func TestServerSpawnCommandWithDir(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	resp, err := SendMessage(sockPath, "spawn:cmd:aider --yes@/tmp/work")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %q", resp)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.commands) != 1 || h.commands[0].command != "aider --yes" || h.commands[0].dir != "/tmp/work" {
		t.Errorf("expected command 'aider --yes' in '/tmp/work', got %v", h.commands)
	}
}

func TestServerSpawnWindow(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	resp, err := SendMessage(sockPath, `spawn-window:{"window":"casts-review","agent":"pi","dir":"/tmp/work"}`)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %q", resp)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.spawns) != 1 || h.spawns[0].window != "casts-review" || h.spawns[0].name != "pi" || h.spawns[0].dir != "/tmp/work" {
		t.Errorf("expected windowed spawn, got %v", h.spawns)
	}
}

func TestSpawnRoleTransitions(t *testing.T) {
	tests := []struct {
		name      string
		requester control.Requester
		message   string
		wantRole  control.Role
		wantError bool
	}{
		{
			name:      "controller defaults to manager",
			requester: control.Requester{PaneID: "%0", Role: control.RoleController},
			message:   "spawn:pi@/tmp",
			wantRole:  control.RoleManager,
		},
		{
			name:      "controller requests worker",
			requester: control.Requester{PaneID: "%0", Role: control.RoleController},
			message:   `spawn-role:{"agent":"pi","dir":"/tmp","role":"worker"}`,
			wantRole:  control.RoleWorker,
		},
		{
			name:      "manager defaults to worker",
			requester: control.Requester{PaneID: "%1", Role: control.RoleManager},
			message:   "spawn:pi@/tmp",
			wantRole:  control.RoleWorker,
		},
		{
			name:      "manager cannot create manager",
			requester: control.Requester{PaneID: "%1", Role: control.RoleManager},
			message:   `spawn-role:{"agent":"pi","dir":"/tmp","role":"manager"}`,
			wantError: true,
		},
		{
			name:      "worker cannot spawn",
			requester: control.Requester{PaneID: "%2", Role: control.RoleWorker},
			message:   "spawn:pi@/tmp",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &mockHandler{requester: tt.requester}
			srv := NewServer("", h)
			_, err := srv.dispatch(tt.message, 123)
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError %v", err, tt.wantError)
			}
			if tt.wantError {
				return
			}
			if len(h.spawns) != 1 || h.spawns[0].role != tt.wantRole {
				t.Fatalf("spawns = %+v, want role %s", h.spawns, tt.wantRole)
			}
		})
	}
}

func TestSplitDirSuffix(t *testing.T) {
	tests := []struct {
		input     string
		wantValue string
		wantDir   string
	}{
		{"claude", "claude", ""},
		{"claude@/home/user", "claude", "/home/user"},
		{"aider --yes@/tmp/work", "aider --yes", "/tmp/work"},
		{"claude@relative", "claude@relative", ""},
		{"@/root", "", "/root"},
	}
	for _, tt := range tests {
		value, dir := splitDirSuffix(tt.input)
		if value != tt.wantValue || dir != tt.wantDir {
			t.Errorf("splitDirSuffix(%q) = (%q, %q), want (%q, %q)",
				tt.input, value, dir, tt.wantValue, tt.wantDir)
		}
	}
}

func TestServerKillPane(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	_, err := SendMessage(sockPath, "kill:3")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.kills) != 1 || h.kills[0] != "3" {
		t.Errorf("expected kill of '3', got %v", h.kills)
	}
}

func TestWorkerRequesterCanOnlyKillOwnPane(t *testing.T) {
	req := control.Requester{Role: control.RoleWorker, PaneID: "%7"}
	if !req.CanKillPane("%7") {
		t.Fatal("worker should be allowed to kill its own pane")
	}
	if req.CanKillPane("%8") {
		t.Fatal("worker should not be allowed to kill another pane")
	}
	if req.CanKillWindow() {
		t.Fatal("worker should not be allowed to kill windows")
	}
}

func TestServerKillWindow(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	_, err := SendMessage(sockPath, "kill-window:casts-review")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.windowKills) != 1 || h.windowKills[0] != "casts-review" {
		t.Errorf("expected kill of casts-review, got %v", h.windowKills)
	}
}

func TestServerRenameWindow(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	_, err := SendMessage(sockPath, `rename-window:{"target":"pi","name":"hammerbound"}`)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.renames) != 1 || h.renames[0].target != "pi" || h.renames[0].name != "hammerbound" {
		t.Errorf("expected rename pi to hammerbound, got %v", h.renames)
	}
}

func TestServerMovePane(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	_, err := SendMessage(sockPath, `move:{"pane":"%7","window":"journalia"}`)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.moves) != 1 || h.moves[0].pane != "%7" || h.moves[0].window != "journalia" {
		t.Errorf("expected move %%7 to journalia, got %v", h.moves)
	}
}

func TestServerMovePaneWorkerDenied(t *testing.T) {
	h := &mockHandler{requester: control.Requester{PaneID: "%1", Role: control.RoleWorker, RootID: "%0"}}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	resp, err := SendMessage(sockPath, `move:{"pane":"%7","window":"journalia"}`)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !strings.Contains(resp, "only move their own pane") {
		t.Errorf("expected denial, got %q", resp)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.moves) != 0 {
		t.Errorf("expected no moves, got %v", h.moves)
	}
}

func TestServerSetLayout(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	_, err := SendMessage(sockPath, "layout:tiled")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.layouts) != 1 || h.layouts[0] != "tiled" {
		t.Errorf("expected layout 'tiled', got %v", h.layouts)
	}
}

func TestServerInvalidMessage(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	resp, err := SendMessage(sockPath, "bogus")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp != "" && resp == "ok" {
		t.Error("expected error response for invalid message")
	}
}

func TestServerUnknownCommand(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	resp, err := SendMessage(sockPath, "restart:all")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp == "ok" {
		t.Error("expected error response for unknown command")
	}
}

func TestServerHandlerError(t *testing.T) {
	h := &mockHandler{failNext: true}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	resp, err := SendMessage(sockPath, "spawn:claude")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp == "ok" {
		t.Error("expected error response when handler fails")
	}
}

func TestServerClose(t *testing.T) {
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Sending after close should fail.
	_, err := SendMessage(sockPath, "spawn:claude")
	if err == nil {
		t.Error("expected error sending to closed server")
	}
}
