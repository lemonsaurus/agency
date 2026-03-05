package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lemonsaurus/agency/internal/config"
)

// MockCommander records calls and returns canned responses.
type MockCommander struct {
	Calls    [][]string
	Returns  map[string]mockReturn
	Default  mockReturn
}

type mockReturn struct {
	Output string
	Err    error
}

func NewMockCommander() *MockCommander {
	return &MockCommander{
		Returns: make(map[string]mockReturn),
	}
}

func (m *MockCommander) key(args []string) string {
	return strings.Join(args, " ")
}

func (m *MockCommander) On(output string, err error, args ...string) {
	m.Returns[m.key(args)] = mockReturn{Output: output, Err: err}
}

func (m *MockCommander) Run(_ context.Context, args ...string) (string, error) {
	m.Calls = append(m.Calls, args)
	if r, ok := m.Returns[m.key(args)]; ok {
		return r.Output, r.Err
	}
	return m.Default.Output, m.Default.Err
}

func (m *MockCommander) Exec(_ context.Context, args ...string) error {
	m.Calls = append(m.Calls, args)
	if r, ok := m.Returns[m.key(args)]; ok {
		return r.Err
	}
	return m.Default.Err
}

func TestSessionExists(t *testing.T) {
	mock := NewMockCommander()
	mock.On("", nil, "has-session", "-t", "test")
	c := &Client{Cmd: mock, SessionName: "test"}

	if !c.SessionExists(context.Background()) {
		t.Error("expected session to exist")
	}
}

func TestSessionNotExists(t *testing.T) {
	mock := NewMockCommander()
	mock.On("", fmt.Errorf("no session"), "has-session", "-t", "test")
	c := &Client{Cmd: mock, SessionName: "test"}

	if c.SessionExists(context.Background()) {
		t.Error("expected session to not exist")
	}
}

func TestNewSession(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	if err := c.NewSession(context.Background()); err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if len(mock.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mock.Calls))
	}
	call := mock.Calls[0]
	if call[0] != "new-session" {
		t.Errorf("expected 'new-session', got %q", call[0])
	}
}

func TestNewSessionWithConfig(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test", ConfigPath: "/tmp/test.conf"}

	if err := c.NewSession(context.Background()); err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	call := mock.Calls[0]
	if call[0] != "-f" || call[1] != "/tmp/test.conf" {
		t.Errorf("expected config args, got %v", call[:2])
	}
}

func TestSplitWindow(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "%5", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	paneID, err := c.SplitWindow(context.Background(), "claude", "")
	if err != nil {
		t.Fatalf("SplitWindow failed: %v", err)
	}
	if paneID != "%5" {
		t.Errorf("expected pane ID '%%5', got %q", paneID)
	}
	// Should not contain -c when dir is empty.
	call := mock.Calls[0]
	for _, arg := range call {
		if arg == "-c" {
			t.Error("unexpected -c flag when dir is empty")
		}
	}
}

func TestSplitWindowWithDir(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "%6", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	paneID, err := c.SplitWindow(context.Background(), "claude", "/home/user/project")
	if err != nil {
		t.Fatalf("SplitWindow with dir failed: %v", err)
	}
	if paneID != "%6" {
		t.Errorf("expected pane ID '%%6', got %q", paneID)
	}
	call := mock.Calls[0]
	foundC := false
	for i, arg := range call {
		if arg == "-c" && i+1 < len(call) && call[i+1] == "/home/user/project" {
			foundC = true
		}
	}
	if !foundC {
		t.Errorf("expected -c /home/user/project in call, got %v", call)
	}
}

func TestSetPaneOption(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	if err := c.SetPaneOption(context.Background(), "%0", "@agent_color", "#f38ba8"); err != nil {
		t.Fatalf("SetPaneOption failed: %v", err)
	}
	call := mock.Calls[0]
	if call[0] != "set-option" || call[1] != "-p" || call[3] != "%0" || call[4] != "@agent_color" || call[5] != "#f38ba8" {
		t.Errorf("unexpected call: %v", call)
	}
}

func TestSetPaneTitle(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	if err := c.SetPaneTitle(context.Background(), "%0", "🤖 claude #1"); err != nil {
		t.Fatalf("SetPaneTitle failed: %v", err)
	}
	call := mock.Calls[0]
	if call[0] != "select-pane" || call[2] != "%0" || call[3] != "-T" || call[4] != "🤖 claude #1" {
		t.Errorf("unexpected call: %v", call)
	}
}

func TestListPanes(t *testing.T) {
	output := "%0\t0\tclaude\t/home/user\t1\t1234\n%1\t1\tcodex\t/home/user/proj\t0\t5678"
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: output, Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	panes, err := c.ListPanes(context.Background())
	if err != nil {
		t.Fatalf("ListPanes failed: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("expected 2 panes, got %d", len(panes))
	}
	if panes[0].ID != "%0" {
		t.Errorf("expected pane ID '%%0', got %q", panes[0].ID)
	}
	if panes[0].Command != "claude" {
		t.Errorf("expected command 'claude', got %q", panes[0].Command)
	}
	if !panes[0].Active {
		t.Error("expected first pane to be active")
	}
	if panes[1].Active {
		t.Error("expected second pane to be inactive")
	}
	if panes[0].PID != 1234 {
		t.Errorf("expected PID 1234, got %d", panes[0].PID)
	}
}

func TestKillPane(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	if err := c.KillPane(context.Background(), "%3"); err != nil {
		t.Fatalf("KillPane failed: %v", err)
	}
	call := mock.Calls[0]
	if call[0] != "kill-pane" || call[2] != "%3" {
		t.Errorf("unexpected call: %v", call)
	}
}

func TestSelectLayout(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	if err := c.SelectLayout(context.Background(), "tiled"); err != nil {
		t.Fatalf("SelectLayout failed: %v", err)
	}
	call := mock.Calls[0]
	if call[0] != "select-layout" || call[3] != "tiled" {
		t.Errorf("unexpected call: %v", call)
	}
}

func TestGenerateConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	content := buildTmuxConf(cfg, "agency")

	checks := []string{
		"set -g prefix " + cfg.Keys.Prefix,
		"set -g mouse on",
		"set -g history-limit 50000",
		"set -g mode-keys emacs",
		"pane-border-status top",
		"bind " + cfg.Keys.Palette + " display-popup",
		"bind " + cfg.Keys.CopyMode + " copy-mode",
		"bind " + cfg.Keys.Paste + " paste-buffer",
		"bind " + cfg.Keys.Terminal + " run-shell",
		"bind 2 display-popup",
		"spawn-dialog",
		"#{b:pane_current_path}",
		"#{@agent_color}",
		"#{@agency_label}",
		"#{pane_current_command}",
		"pane-active-border-style",
		"terminal-features 'xterm*:hyperlinks'",
		"allow-passthrough on",
		"bind Up select-pane -U",
		"bind " + cfg.Keys.LayoutTiled + " run-shell",
		"bind " + cfg.Keys.KillPane + " display-popup",
		"bind " + cfg.Keys.KillSession + " display-popup",
		"Kill session",
		"bind " + cfg.Keys.Zoom + " resize-pane -Z",
		cfg.Theme.ActiveBorder,
		cfg.Theme.InactiveBorder,
		cfg.Theme.StatusBG,
	}

	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("tmux.conf missing expected content: %q", check)
		}
	}
}

func TestGenerateConfigCustomKeys(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Keys.Prefix = "C-Space"
	cfg.Keys.Palette = "p"
	cfg.Keys.Zoom = "f"
	cfg.Keys.CopyMode = "c"
	cfg.Keys.Paste = "v"
	content := buildTmuxConf(cfg, "agency")

	checks := []string{
		"set -g prefix C-Space",
		"bind C-Space send-prefix",
		"bind p display-popup",
		"bind f resize-pane -Z",
		"bind c copy-mode",
		"bind v paste-buffer",
	}

	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("tmux.conf missing expected content: %q", check)
		}
	}
}

func TestCapturePaneContent(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "some output\nfrom pane", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	out, err := c.CapturePaneContent(context.Background(), "%0", 30)
	if err != nil {
		t.Fatalf("CapturePaneContent failed: %v", err)
	}
	if out != "some output\nfrom pane" {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestSetEnv(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	if err := c.SetEnv(context.Background(), "AGENCY_SOCKET", "/tmp/agency.sock"); err != nil {
		t.Fatalf("SetEnv failed: %v", err)
	}
	call := mock.Calls[0]
	if call[3] != "AGENCY_SOCKET" || call[4] != "/tmp/agency.sock" {
		t.Errorf("unexpected call: %v", call)
	}
}
