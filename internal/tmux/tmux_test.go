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
	Calls   [][]string
	Returns map[string]mockReturn
	Default mockReturn
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
	if !strings.Contains(strings.Join(call, " "), "-n control") {
		t.Errorf("expected control window name, got %v", call)
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

func TestSourceConfig(t *testing.T) {
	mock := NewMockCommander()
	c := &Client{Cmd: mock, ConfigPath: "/tmp/test.conf"}

	if err := c.SourceConfig(context.Background()); err != nil {
		t.Fatalf("SourceConfig failed: %v", err)
	}
	if got := strings.Join(mock.Calls[0], " "); got != "source-file /tmp/test.conf" {
		t.Errorf("unexpected command: %s", got)
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

func TestNewWindow(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "%7", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	paneID, err := c.NewWindow(context.Background(), "casts-review", "pi", "/tmp/project")
	if err != nil {
		t.Fatalf("NewWindow failed: %v", err)
	}
	if paneID != "%7" {
		t.Errorf("expected pane ID '%%7', got %q", paneID)
	}
	call := mock.Calls[0]
	want := []string{"new-window", "-t", "test:", "-n", "casts-review", "-P", "-F", "#{pane_id}", "-c", "/tmp/project", "pi"}
	if strings.Join(call, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("unexpected call: %v", call)
	}
}

func TestSplitWindowInWindow(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "%8", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	paneID, err := c.SplitWindowInWindow(context.Background(), "casts-review", "pi", "/tmp/project")
	if err != nil {
		t.Fatalf("SplitWindowInWindow failed: %v", err)
	}
	if paneID != "%8" {
		t.Errorf("expected pane ID '%%8', got %q", paneID)
	}
	want := []string{"split-window", "-t", "test:casts-review", "-P", "-F", "#{pane_id}", "-c", "/tmp/project", "pi"}
	if strings.Join(mock.Calls[0], "\x00") != strings.Join(want, "\x00") {
		t.Errorf("unexpected call: %v", mock.Calls[0])
	}
}

func TestWindowExists(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "pi\ncasts-review", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	exists, err := c.WindowExists(context.Background(), "casts-review")
	if err != nil {
		t.Fatalf("WindowExists failed: %v", err)
	}
	if !exists {
		t.Fatal("expected window to exist")
	}
}

func TestNameFirstWindow(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "@4\t4\tlater\n@1\t1\tfirst", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	if err := c.NameFirstWindow(context.Background(), "control"); err != nil {
		t.Fatalf("NameFirstWindow failed: %v", err)
	}
	if len(mock.Calls) < 2 || strings.Join(mock.Calls[1], " ") != "rename-window -t @1 control" {
		t.Fatalf("rename call = %v", mock.Calls)
	}
}

func TestSplitWindowInFirstWindow(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "@4\t4\tlater\n@1\t1\tfirst", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	_, windowName, err := c.SplitWindowInFirstWindow(context.Background(), "pi", "/tmp")
	if err != nil {
		t.Fatalf("SplitWindowInFirstWindow failed: %v", err)
	}
	if windowName != "first" {
		t.Fatalf("first window = %q, want first", windowName)
	}
	if len(mock.Calls) < 2 || mock.Calls[1][2] != "@1" {
		t.Fatalf("expected split target @1, calls: %v", mock.Calls)
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
	output := "1\tπ pi\t%0\t0\tpi\t/home/user\t1\t1234\n2\tcasts-review\t%1\t1\tcodex\t/home/user/proj\t0\t5678"
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
	if panes[0].WindowName != "π pi" {
		t.Errorf("expected unicode window name, got %q", panes[0].WindowName)
	}
	if panes[0].Command != "pi" {
		t.Errorf("expected command 'pi', got %q", panes[0].Command)
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

func TestListPanesIncludesControlState(t *testing.T) {
	output := "1\tmain\t%2\t0\tpi\t/tmp\t1\t1234\tworker\t%1\t%0\tbmVlZCBmYW5vdXQ\tZWNobyBoaQ"
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: output, Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	panes, err := c.ListPanes(context.Background())
	if err != nil || len(panes) != 1 {
		t.Fatalf("ListPanes = (%v, %v)", panes, err)
	}
	pane := panes[0]
	if pane.Role != "worker" || pane.ParentID != "%1" || pane.RootID != "%0" || pane.PendingPromotion != "bmVlZCBmYW5vdXQ" || pane.AgencyCommand != "ZWNobyBoaQ" {
		t.Fatalf("unexpected control state: %+v", pane)
	}
}

func TestSendText(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	if err := c.SendText(context.Background(), "%3", "hello", true); err != nil {
		t.Fatalf("SendText failed: %v", err)
	}
	call := mock.Calls[0]
	if strings.Join(call, " ") != "send-keys -l -t %3 hello\r" {
		t.Errorf("unexpected call: %v", call)
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

func TestKillWindow(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	if err := c.KillWindow(context.Background(), "casts-review"); err != nil {
		t.Fatalf("KillWindow failed: %v", err)
	}
	call := mock.Calls[0]
	if call[0] != "kill-window" || call[2] != "test:casts-review" {
		t.Errorf("unexpected call: %v", call)
	}
}

func TestRenameWindow(t *testing.T) {
	tests := []struct {
		target string
		want   string
	}{
		{target: "casts-review", want: "@2"},
		{target: "2", want: "@2"},
		{target: "@2", want: "@2"},
	}
	for _, tt := range tests {
		mock := NewMockCommander()
		mock.Default = mockReturn{Output: "@1\t1\tpi\n@2\t2\tcasts-review", Err: nil}
		c := &Client{Cmd: mock, SessionName: "test"}

		if err := c.RenameWindow(context.Background(), tt.target, "hammerbound"); err != nil {
			t.Fatalf("RenameWindow(%q) failed: %v", tt.target, err)
		}
		call := mock.Calls[1]
		if call[0] != "rename-window" || call[2] != tt.want || call[3] != "hammerbound" {
			t.Errorf("unexpected call for target %q: %v", tt.target, call)
		}
	}
}

func TestMoveWindow(t *testing.T) {
	tests := []struct {
		name   string
		target string
		index  int
		want   []string
	}{
		{name: "before occupant", target: "Journalia", index: 2, want: []string{"move-window", "-b", "-s", "@5", "-t", "@2"}},
		{name: "after occupant", target: "2", index: 3, want: []string{"move-window", "-a", "-s", "@2", "-t", "@3"}},
		{name: "free index", target: "@2", index: 9, want: []string{"move-window", "-s", "@2", "-t", "test:9"}},
		{name: "already there", target: "@5", index: 5},
	}
	for _, tt := range tests {
		mock := NewMockCommander()
		mock.Default = mockReturn{Output: "@1\t1\tcontrol\n@2\t2\tsidequests\n@3\t3\tenterprise\n@5\t5\tJournalia", Err: nil}
		c := &Client{Cmd: mock, SessionName: "test"}

		if err := c.MoveWindow(context.Background(), tt.target, tt.index); err != nil {
			t.Fatalf("%s: MoveWindow failed: %v", tt.name, err)
		}
		var call []string
		for _, candidate := range mock.Calls {
			if candidate[0] == "move-window" {
				call = candidate
			}
		}
		if fmt.Sprint(call) != fmt.Sprint(tt.want) {
			t.Errorf("%s: unexpected call: %v, want %v", tt.name, call, tt.want)
		}
	}
}

func TestSelectLayoutForWindow(t *testing.T) {
	mock := NewMockCommander()
	mock.Default = mockReturn{Output: "", Err: nil}
	c := &Client{Cmd: mock, SessionName: "test"}

	if err := c.SelectLayoutForWindow(context.Background(), "%5", "tiled"); err != nil {
		t.Fatalf("SelectLayoutForWindow failed: %v", err)
	}
	call := mock.Calls[0]
	if call[0] != "select-layout" || call[2] != "%5" || call[3] != "tiled" {
		t.Errorf("unexpected call: %v", call)
	}
}

func TestGenerateConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	content := buildTmuxConf(cfg, "agency")

	checks := []string{
		"set -g prefix " + cfg.Keys.Prefix,
		"set -g mouse on",
		"set -s set-clipboard on",
		"set -g history-limit 50000",
		"set -g mode-keys emacs",
		"pane-border-status top",
		"bind " + cfg.Keys.Palette + " display-popup",
		"set -as terminal-features 'xterm*:clipboard'",
		"unbind -n Home",
		"unbind -n End",
		"bind -T root MouseDown3Pane display-menu -t = -x M -y M",
		"set -g menu-style bg=" + cfg.Theme.StatusBG + ",fg=" + cfg.Theme.StatusFG,
		"set -g menu-selected-style bg=" + cfg.Theme.ActiveBorder,
		"set -g menu-border-style fg=" + cfg.Theme.ActiveBorder,
		"set -g menu-border-lines rounded",
		"×  #{?@agency_cloud,Destroy,Kill}",
		"bind " + cfg.Keys.CopyMode + " copy-mode",
		"bind " + cfg.Keys.Paste + " paste-buffer",
		"bind -n C-v if -F '#{@agency_cloud}'",
		"cloud-act paste-image #{@agency_cloud}",
		"Paste Image",
		"bind " + cfg.Keys.Terminal + " if -F '#{@agency_cloud}'",
		"cloud-act spawn #{@agency_cloud} --cmd '\\$SHELL'",
		"cloud-act kill #{@agency_cloud}",
		"cloud-act approve #{@agency_cloud}",
		"Close View",
		"-e 'AGENCY_CLOUD_WINDOW=#{@agency_cloud}'",
		"bind 2 display-popup",
		"spawn-dialog",
		"#{b:pane_current_path}",
		"#{@agent_color}",
		"#{@agency_label}",
		"pane-active-border-style",
		"terminal-features 'xterm*:hyperlinks'",
		"terminal-features 'tmux*:hyperlinks'",
		"bind Up select-pane -U",
		"bind " + cfg.Keys.LayoutTiled + " run-shell",
		"bind " + cfg.Keys.KillPane + " if -F '#{@agency_cloud}'",
		"confirm-before -y -p 'Kill pane? (y/n)' kill-pane",
		"bind " + cfg.Keys.KillSession + " confirm-before -y -p 'Kill session? (y/n)' \"run-shell 'tmux kill-session -t =remote-agency; tmux kill-session -t =agency'\"",
		"bind " + cfg.Keys.World + " run-shell -b \"agency world '#{client_name}' '#{session_name}'\"",
		"bind -T root MouseUp1StatusLeft run-shell -b \"agency world",
		"bind -T root MouseDown3StatusLeft display-menu",
		"bind -T root MouseDown3Status display-menu",
		"bind " + cfg.Keys.NewWindow + " command-prompt -p 'New window:'",
		"new-window '#{session_name}' '%%' '#{pane_current_path}'",
		"rename-window '#{window_id}' '%%'",
		"send-to-sky '#{client_name}' '#{window_id}'",
		"#{?#{==:#{session_name},remote-agency},#[bg=#fab387#,fg=#1e1e2e#,bold]☁  sky  ,#[bg=#a6e3a1#,fg=#1e1e2e#,bold]♁ earth  }",
		"bind " + cfg.Keys.Zoom + " resize-pane -Z",
		"bind " + cfg.Keys.ApprovePromotion + " if -F '#{@agency_cloud}'",
		"Promote worker #{pane_id} to manager? (y/n)",
		"promotion pending: Prefix+" + cfg.Keys.ApprovePromotion,
		"approve-promotion #{pane_id}",
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
	call := mock.Calls[0]
	if strings.Join(call, " ") != "capture-pane -t %0 -p -S -30" {
		t.Errorf("unexpected call: %v", call)
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
