package ipc

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

type spawnRecord struct {
	name string
	dir  string
}

type commandRecord struct {
	command string
	dir     string
}

type mockHandler struct {
	mu       sync.Mutex
	spawns   []spawnRecord
	commands []commandRecord
	kills    []string
	layouts  []string
	failNext bool
}

func (m *mockHandler) SpawnAgent(_ context.Context, name, dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext {
		m.failNext = false
		return fmt.Errorf("spawn failed")
	}
	m.spawns = append(m.spawns, spawnRecord{name: name, dir: dir})
	return nil
}

func (m *mockHandler) SpawnCommand(_ context.Context, command, dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands = append(m.commands, commandRecord{command: command, dir: dir})
	return nil
}

func (m *mockHandler) KillPane(_ context.Context, paneID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kills = append(m.kills, paneID)
	return nil
}

func (m *mockHandler) SetLayout(_ context.Context, layout string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.layouts = append(m.layouts, layout)
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
