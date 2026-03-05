package session

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lemonsaurus/agency/internal/agents"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// testMock implements tmux.Commander for session tests.
type testMock struct {
	calls      [][]string
	paneIDSeq  int
	listOutput string
}

func (m *testMock) Run(_ context.Context, args ...string) (string, error) {
	m.calls = append(m.calls, args)
	key := args[0]

	switch key {
	case "split-window":
		m.paneIDSeq++
		return fmt.Sprintf("%%%d", m.paneIDSeq), nil
	case "list-panes":
		return m.listOutput, nil
	case "display-message":
		// Return fake window info for custom tiled layout.
		return "200\t50\t" + fmt.Sprintf("%d", m.paneIDSeq+1), nil
	case "send-keys":
		return "", nil
	case "select-layout":
		return "", nil
	case "kill-pane":
		return "", nil
	case "set-environment":
		return "", nil
	case "set-option":
		return "", nil
	case "select-pane":
		return "", nil
	}

	return "", nil
}

func (m *testMock) Exec(_ context.Context, args ...string) error {
	m.calls = append(m.calls, args)
	return nil
}

func (m *testMock) findCall(prefix string) []string {
	for _, c := range m.calls {
		if len(c) > 0 && c[0] == prefix {
			return c
		}
	}
	return nil
}

func (m *testMock) findCalls(prefix string) [][]string {
	var results [][]string
	for _, c := range m.calls {
		if len(c) > 0 && c[0] == prefix {
			results = append(results, c)
		}
	}
	return results
}

func newTestManager(mock *testMock) *Manager {
	tc := &tmux.Client{
		Cmd:         mock,
		SessionName: "test",
	}
	agentMap := map[string]config.AgentConfig{
		"claude": {Command: "claude", Icon: "🤖", BorderColor: "#cba6f7"},
		"codex":  {Command: "codex", Icon: "🧠", BorderColor: "#89b4fa"},
	}
	reg := agents.NewRegistry(agentMap, []string{"claude", "codex"})
	cfg := config.DefaultConfig()
	return NewManager(tc, reg, cfg, nil)
}

func TestSpawnAgent(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	if err := mgr.SpawnAgent(context.Background(), "claude", ""); err != nil {
		t.Fatalf("SpawnAgent failed: %v", err)
	}

	// Always uses split-window.
	splitCall := mock.findCall("split-window")
	if splitCall == nil {
		t.Fatal("expected split-window call")
	}

	panes := mgr.ListPanes()
	if len(panes) != 1 {
		t.Fatalf("expected 1 pane, got %d", len(panes))
	}
	if panes[0].AgentType != "claude" {
		t.Errorf("expected agent type 'claude', got %q", panes[0].AgentType)
	}
	if !strings.Contains(panes[0].AgentName, "claude") {
		t.Errorf("expected display name to contain 'claude', got %q", panes[0].AgentName)
	}

	// Should have set @agency_label and @agent_color via set-option.
	optionCalls := mock.findCalls("set-option")
	foundLabel, foundColor := false, false
	for _, c := range optionCalls {
		for _, arg := range c {
			if arg == "@agency_label" {
				foundLabel = true
			}
			if arg == "@agent_color" {
				foundColor = true
			}
		}
	}
	if !foundLabel {
		t.Error("expected set-option call for @agency_label")
	}
	if !foundColor {
		t.Error("expected set-option call for @agent_color")
	}
}

func TestSpawnAgentWithDir(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	if err := mgr.SpawnAgent(context.Background(), "claude", "/tmp/project"); err != nil {
		t.Fatalf("SpawnAgent with dir failed: %v", err)
	}

	// split-window should have -c /tmp/project
	splitCall := mock.findCall("split-window")
	if splitCall == nil {
		t.Fatal("expected split-window call")
	}
	foundC := false
	for i, arg := range splitCall {
		if arg == "-c" && i+1 < len(splitCall) && splitCall[i+1] == "/tmp/project" {
			foundC = true
		}
	}
	if !foundC {
		t.Errorf("expected -c /tmp/project in split-window call, got %v", splitCall)
	}
}

func TestSpawnTwoPanes(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	if err := mgr.SpawnAgent(context.Background(), "claude", ""); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	if err := mgr.SpawnAgent(context.Background(), "codex", ""); err != nil {
		t.Fatalf("second spawn: %v", err)
	}

	panes := mgr.ListPanes()
	if len(panes) != 2 {
		t.Fatalf("expected 2 panes, got %d", len(panes))
	}
}

func TestSpawnUnknownAgent(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	err := mgr.SpawnAgent(context.Background(), "nonexistent", "")
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

func TestSpawnCommand(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	if err := mgr.SpawnCommand(context.Background(), "htop", ""); err != nil {
		t.Fatalf("SpawnCommand failed: %v", err)
	}

	panes := mgr.ListPanes()
	if len(panes) != 1 {
		t.Fatalf("expected 1 pane, got %d", len(panes))
	}
	if panes[0].Command != "htop" {
		t.Errorf("expected command 'htop', got %q", panes[0].Command)
	}
}

func TestKillPane(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	_ = mgr.SpawnAgent(context.Background(), "claude", "")
	panes := mgr.ListPanes()
	if len(panes) != 1 {
		t.Fatalf("expected 1 pane, got %d", len(panes))
	}

	paneID := panes[0].PaneID
	if err := mgr.KillPane(context.Background(), paneID); err != nil {
		t.Fatalf("KillPane failed: %v", err)
	}

	if mgr.PaneCount() != 0 {
		t.Errorf("expected 0 panes after kill, got %d", mgr.PaneCount())
	}
}

func TestKillAll(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	_ = mgr.SpawnAgent(context.Background(), "claude", "")
	_ = mgr.SpawnAgent(context.Background(), "codex", "")
	if mgr.PaneCount() != 2 {
		t.Fatalf("expected 2 panes, got %d", mgr.PaneCount())
	}

	if err := mgr.KillAll(context.Background()); err != nil {
		t.Fatalf("KillAll failed: %v", err)
	}
	if mgr.PaneCount() != 0 {
		t.Errorf("expected 0 panes after kill-all, got %d", mgr.PaneCount())
	}
}

func TestSetLayout(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	if err := mgr.SetLayout(context.Background(), "columns"); err != nil {
		t.Fatalf("SetLayout failed: %v", err)
	}

	layoutCall := mock.findCall("select-layout")
	if layoutCall == nil {
		t.Fatal("expected select-layout call")
	}
	// "columns" maps to "even-horizontal".
	found := false
	for _, arg := range layoutCall {
		if arg == "even-horizontal" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'even-horizontal' in layout call, got %v", layoutCall)
	}
}

func TestTmuxLayout(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"tiled", "tiled"},
		{"columns", "even-horizontal"},
		{"rows", "even-vertical"},
		{"main-vertical", "main-vertical"},
		{"unknown", "tiled"},
	}

	for _, tt := range tests {
		got := tmuxLayout(tt.input)
		if got != tt.expected {
			t.Errorf("tmuxLayout(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestAdoptOrphans(t *testing.T) {
	mock := &testMock{
		listOutput: "%0\t0\tclaude\t/home/user/myproject\t1\t1234\n%1\t1\tcodex\t/home/user/backend\t0\t5678",
	}
	mgr := newTestManager(mock)

	if err := mgr.AdoptOrphans(context.Background()); err != nil {
		t.Fatalf("AdoptOrphans failed: %v", err)
	}

	if mgr.PaneCount() != 2 {
		t.Fatalf("expected 2 adopted panes, got %d", mgr.PaneCount())
	}

	panes := mgr.ListPanes()
	types := make(map[string]bool)
	names := make(map[string]bool)
	for _, p := range panes {
		types[p.AgentType] = true
		names[p.AgentName] = true
	}
	if !types["claude"] {
		t.Error("expected 'claude' agent to be adopted")
	}
	if !types["codex"] {
		t.Error("expected 'codex' agent to be adopted")
	}
	// Labels should use folder names from CWD.
	if !names["🤖 claude@myproject"] {
		t.Errorf("expected 'claude@myproject' label, got %v", names)
	}
	if !names["🧠 codex@backend"] {
		t.Errorf("expected 'codex@backend' label, got %v", names)
	}
}

func TestSpawnAgentFolderLabel(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	if err := mgr.SpawnAgent(context.Background(), "claude", "/home/user/myproject"); err != nil {
		t.Fatalf("SpawnAgent failed: %v", err)
	}

	panes := mgr.ListPanes()
	if len(panes) != 1 {
		t.Fatalf("expected 1 pane, got %d", len(panes))
	}
	if panes[0].AgentName != "🤖 claude@myproject" {
		t.Errorf("expected 'claude@myproject', got %q", panes[0].AgentName)
	}
}

func TestFolderLabel(t *testing.T) {
	tests := []struct {
		dir  string
		want string
	}{
		{"/home/user/myproject", "myproject"},
		{"/home/user/my-app", "my-app"},
		{"", ""},
		{"/", ""},
		{".", ""},
	}
	for _, tt := range tests {
		got := folderLabel(tt.dir)
		if got != tt.want {
			t.Errorf("folderLabel(%q) = %q, want %q", tt.dir, got, tt.want)
		}
	}
}

func TestPaletteColors(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	// Spawn 3 panes — each should get a different color.
	_ = mgr.SpawnAgent(context.Background(), "claude", "/proj/a")
	_ = mgr.SpawnAgent(context.Background(), "claude", "/proj/b")
	_ = mgr.SpawnAgent(context.Background(), "claude", "/proj/c")

	// Each pane gets its own set-option calls. Collect all @agent_color values.
	colors := map[string]bool{}
	for _, call := range mock.findCalls("set-option") {
		for i, arg := range call {
			if arg == "@agent_color" && i+1 < len(call) {
				colors[call[i+1]] = true
			}
		}
	}
	if len(colors) != 3 {
		t.Errorf("expected 3 distinct pane colors, got %d: %v", len(colors), colors)
	}
}

func TestInstanceCounters(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	_ = mgr.SpawnAgent(context.Background(), "claude", "")
	_ = mgr.SpawnAgent(context.Background(), "claude", "")
	_ = mgr.SpawnAgent(context.Background(), "claude", "")

	panes := mgr.ListPanes()
	if len(panes) != 3 {
		t.Fatalf("expected 3 panes, got %d", len(panes))
	}

	names := make(map[string]bool)
	for _, p := range panes {
		names[p.AgentName] = true
	}
	if !names["🤖 claude #1"] {
		t.Error("expected 'claude #1'")
	}
	if !names["🤖 claude #2"] {
		t.Error("expected 'claude #2'")
	}
	if !names["🤖 claude #3"] {
		t.Error("expected 'claude #3'")
	}
}

func TestAdoptOrphansSetsLabels(t *testing.T) {
	mock := &testMock{
		listOutput: "%0\t0\tclaude\t/home/user\t1\t1234\n%1\t1\tcodex\t/home/user\t0\t5678",
	}
	mgr := newTestManager(mock)

	if err := mgr.AdoptOrphans(context.Background()); err != nil {
		t.Fatalf("AdoptOrphans failed: %v", err)
	}

	// Should have called set-option for @agency_label and @agent_color on each adopted pane.
	optionCalls := mock.findCalls("set-option")
	labelCount, colorCount := 0, 0
	for _, c := range optionCalls {
		for _, arg := range c {
			if arg == "@agency_label" {
				labelCount++
			}
			if arg == "@agent_color" {
				colorCount++
			}
		}
	}
	if labelCount < 2 {
		t.Errorf("expected at least 2 @agency_label set-option calls, got %d", labelCount)
	}
	if colorCount < 2 {
		t.Errorf("expected at least 2 @agent_color set-option calls, got %d", colorCount)
	}
}
