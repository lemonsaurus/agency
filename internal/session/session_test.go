package session

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lemonsaurus/agency/internal/agents"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// testMock implements tmux.Commander for session tests.
type testMock struct {
	calls            [][]string
	paneIDSeq        int
	listOutput       string
	windowOutput     string
	windowInfoOutput string
}

func (m *testMock) Run(_ context.Context, args ...string) (string, error) {
	m.calls = append(m.calls, args)
	key := args[0]

	switch key {
	case "split-window", "new-window":
		m.paneIDSeq++
		return fmt.Sprintf("%%%d", m.paneIDSeq), nil
	case "list-panes":
		return m.listOutput, nil
	case "list-windows":
		return m.windowOutput, nil
	case "display-message":
		for _, a := range args {
			if a == "#{pid}" {
				return "42", nil // fake tmux server PID
			}
		}
		if m.windowInfoOutput != "" {
			return m.windowInfoOutput, nil
		}
		// Return fake window info for custom tiled layout.
		return "200\t50\t" + fmt.Sprintf("%d", m.paneIDSeq+1), nil
	case "send-keys":
		return "", nil
	case "select-layout":
		return "", nil
	case "kill-pane", "kill-window", "rename-window":
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

var testController = control.Requester{PaneID: "%0", Role: control.RoleController, RootID: "%0"}

func TestSpawnAgent(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	if err := mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", ""); err != nil {
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
	roleSet := false
	for _, c := range optionCalls {
		for _, arg := range c {
			if arg == "@agency_role" {
				roleSet = true
			}
		}
	}
	if !roleSet {
		t.Error("expected set-option call for @agency_role")
	}
}

func TestHumanSpawnCreatesControllerInFirstWindow(t *testing.T) {
	mock := &testMock{windowOutput: "@4\t4\tlater\n@1\t1\tfirst"}
	mgr := newTestManager(mock)
	human := control.Requester{Role: control.RoleController, Human: true}

	if err := mgr.SpawnAgent(context.Background(), human, control.RoleController, "claude", "/tmp/project"); err != nil {
		t.Fatalf("SpawnAgent failed: %v", err)
	}
	splitCall := mock.findCall("split-window")
	if splitCall == nil || splitCall[2] != "@1" {
		t.Fatalf("expected first window target @1, got %v", splitCall)
	}
	pane := mgr.ListPanes()[0]
	if pane.Role != control.RoleController || pane.ParentID != "" || pane.RootID != pane.PaneID || pane.WindowName != "first" {
		t.Fatalf("unexpected controller metadata: %+v", pane)
	}
}

func TestHumanSpawnHonorsRequestedWindow(t *testing.T) {
	mock := &testMock{windowOutput: "journalia", windowInfoOutput: "200\t50\t2"}
	mgr := newTestManager(mock)
	human := control.Requester{Role: control.RoleController, Human: true}

	if err := mgr.SpawnCommandWindow(context.Background(), human, control.RoleController, "journalia", "/usr/bin/zsh", "/tmp/project"); err != nil {
		t.Fatalf("SpawnCommandWindow failed: %v", err)
	}
	splitCall := mock.findCall("split-window")
	if splitCall == nil || splitCall[2] != "test:journalia" {
		t.Fatalf("expected target test:journalia, got %v", splitCall)
	}
	if pane := mgr.ListPanes()[0]; pane.Role != control.RoleController || pane.WindowName != "journalia" {
		t.Fatalf("unexpected controller metadata: %+v", pane)
	}
}

func TestSpawnAgentWithDir(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	if err := mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "/tmp/project"); err != nil {
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

	if err := mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", ""); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	if err := mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "codex", ""); err != nil {
		t.Fatalf("second spawn: %v", err)
	}

	panes := mgr.ListPanes()
	if len(panes) != 2 {
		t.Fatalf("expected 2 panes, got %d", len(panes))
	}
}

func TestSpawnLimits(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)
	mgr.cfg.Session.MaxManagers = 1
	if err := mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", ""); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", ""); err == nil {
		t.Fatal("expected manager limit error")
	}

	mgr.cfg.Session.MaxWorkersPerManager = 1
	manager := control.Requester{PaneID: "%1", Role: control.RoleManager, RootID: "%0"}
	if err := mgr.SpawnAgent(context.Background(), manager, control.RoleWorker, "claude", ""); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SpawnAgent(context.Background(), manager, control.RoleWorker, "claude", ""); err == nil {
		t.Fatal("expected worker limit error")
	}
}

func TestSpawnUnknownAgent(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	err := mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "nonexistent", "")
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

func TestSpawnCommand(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	if err := mgr.SpawnCommand(context.Background(), testController, control.RoleWorker, "htop", ""); err != nil {
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

func TestSpawnAgentWindow(t *testing.T) {
	mock := &testMock{windowInfoOutput: "200\t50\t1"}
	mgr := newTestManager(mock)

	if err := mgr.SpawnAgentWindow(context.Background(), testController, control.RoleManager, "casts-review", "claude", "/tmp/project"); err != nil {
		t.Fatalf("SpawnAgentWindow failed: %v", err)
	}

	if mock.findCall("new-window") == nil {
		t.Fatal("expected new-window call")
	}
	if mock.findCall("select-layout") != nil {
		t.Fatal("did not expect relayout for a task window")
	}
	panes := mgr.ListPanes()
	if len(panes) != 1 {
		t.Fatalf("expected 1 pane, got %d", len(panes))
	}
	if panes[0].WindowName != "casts-review" {
		t.Errorf("expected window name, got %q", panes[0].WindowName)
	}
}

func TestSpawnAgentWindowReusesExistingWindow(t *testing.T) {
	mock := &testMock{
		windowOutput:     "casts-review",
		windowInfoOutput: "200\t50\t2",
	}
	mgr := newTestManager(mock)

	if err := mgr.SpawnAgentWindow(context.Background(), testController, control.RoleManager, "casts-review", "claude", "/tmp/project"); err != nil {
		t.Fatalf("SpawnAgentWindow failed: %v", err)
	}

	if mock.findCall("new-window") != nil {
		t.Fatal("did not expect new-window call")
	}
	splitCall := mock.findCall("split-window")
	if splitCall == nil {
		t.Fatal("expected split-window call")
	}
	if splitCall[2] != "test:casts-review" {
		t.Errorf("expected target test:casts-review, got %v", splitCall)
	}
	layoutCall := mock.findCall("select-layout")
	if layoutCall == nil {
		t.Fatal("expected relayout after spawning into an existing window")
	}
	if layoutCall[2] != "%1" {
		t.Errorf("expected layout target %%1, got %v", layoutCall)
	}
}

func TestReplaceManagerTransfersChildren(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)
	if err := mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "/tmp/manager"); err != nil {
		t.Fatal(err)
	}
	manager := control.Requester{PaneID: "%1", Role: control.RoleManager, RootID: "%0"}
	if err := mgr.SpawnAgent(context.Background(), manager, control.RoleWorker, "codex", "/tmp/worker"); err != nil {
		t.Fatal(err)
	}
	mock.listOutput = "1\twork\t%1\t0\tclaude\t/tmp/manager\t1\t101\tmanager\t%0\t%0\t\n1\twork\t%2\t1\tcodex\t/tmp/worker\t0\t102\tworker\t%1\t%0\t"
	mgr.cfg.Session.MaxPanes = 2
	mgr.cfg.Session.MaxManagers = 1

	replacementID, err := mgr.ReplacePane(context.Background(), manager, "handoff-pi", "/tmp/replacement")
	if err != nil {
		t.Fatalf("ReplacePane: %v", err)
	}
	calls := mock.findCalls("split-window")
	replacementCall := calls[len(calls)-1]
	if replacementCall[len(replacementCall)-3] != "-c" || replacementCall[len(replacementCall)-2] != "/tmp/replacement" || !strings.Contains(replacementCall[len(replacementCall)-1], "handoff-pi") {
		t.Fatalf("replacement split = %v", replacementCall)
	}
	if replacementID != "%3" {
		t.Fatalf("replacement ID = %s, want %%3", replacementID)
	}
	panes := mgr.ListPanes()
	byID := make(map[string]TrackedPane)
	for _, pane := range panes {
		byID[pane.PaneID] = pane
	}
	if byID["%2"].ParentID != "%3" {
		t.Fatalf("child parent = %s, want %%3", byID["%2"].ParentID)
	}
	if byID["%3"].Role != control.RoleManager || byID["%3"].RootID != "%0" {
		t.Fatalf("unexpected replacement: %+v", byID["%3"])
	}
}

func TestReplaceWorkerPreservesAuthorityAtCapacity(t *testing.T) {
	mock := &testMock{listOutput: "1\troot\t%0\t0\tpi\t/tmp/root\t1\t100\tcontroller\t\t%0\t\n2\twork\t%5\t0\tpi\t/tmp/worker\t1\t105\tworker\t%4\t%0\tbmVlZCBmYW5vdXQ\n"}
	mgr := newTestManager(mock)
	if err := mgr.AdoptOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.cfg.Session.MaxPanes = 1
	mgr.cfg.Session.MaxWorkersPerManager = 1
	worker := control.Requester{PaneID: "%5", Role: control.RoleWorker, RootID: "%0"}
	id, err := mgr.ReplacePane(context.Background(), worker, "handoff-pi", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, pane := range mgr.ListPanes() {
		if pane.PaneID == id && (pane.Role != control.RoleWorker || pane.ParentID != "%4" || pane.RootID != "%0" || pane.PendingPromotion != "need fanout") {
			t.Fatalf("replacement changed authority: %+v", pane)
		}
	}
	call := mock.findCall("split-window")
	if call[2] != "%5" || !strings.Contains(strings.Join(call, " "), "/tmp/worker") {
		t.Fatalf("replacement did not preserve location: %v", call)
	}
}

func TestReplaceControllerUpdatesDescendantRoots(t *testing.T) {
	mock := &testMock{listOutput: "1\tmain\t%0\t0\tpi\t/tmp/root\t1\t100\tcontroller\t\t%0\t\n1\tmain\t%5\t1\tpi\t/tmp/manager\t0\t105\tmanager\t%0\t%0\t"}
	mgr := newTestManager(mock)
	if err := mgr.AdoptOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	replacementID, err := mgr.ReplacePane(context.Background(), control.Requester{PaneID: "%0", Role: control.RoleController, RootID: "%0"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if replacementID != "%1" {
		t.Fatalf("replacement ID = %s, want %%1", replacementID)
	}
	byID := make(map[string]TrackedPane)
	for _, pane := range mgr.ListPanes() {
		byID[pane.PaneID] = pane
	}
	if byID["%5"].ParentID != "%1" || byID["%5"].RootID != "%1" {
		t.Fatalf("descendant was not transferred: %+v", byID["%5"])
	}
	if byID["%1"].ParentID != "" || byID["%1"].RootID != "%1" {
		t.Fatalf("unexpected controller replacement: %+v", byID["%1"])
	}
}

func TestWorkerPromotionLifecycle(t *testing.T) {
	mock := &testMock{listOutput: "1\troot\t%0\t0\tpi\t/tmp/root\t1\t100\tcontroller\t\t%0\t\n2\tmanager\t%1\t0\tpi\t/tmp/manager\t1\t101\tmanager\t%0\t%0\t\n3\tworker\t%2\t0\tpi\t/tmp/worker\t1\t102\tworker\t%1\t%0\t", windowOutput: "manager"}
	mgr := newTestManager(mock)
	if err := mgr.AdoptOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	worker := control.Requester{PaneID: "%2", Role: control.RoleWorker, RootID: "%0"}
	if err := mgr.RequestPromotion(context.Background(), worker, "need two workers"); err != nil {
		t.Fatalf("RequestPromotion: %v", err)
	}
	for _, role := range []control.Role{control.RoleController, control.RoleManager, control.RoleWorker} {
		if err := mgr.ApprovePromotion(context.Background(), control.Requester{Role: role}, "%2"); err == nil {
			t.Fatalf("expected %s agent approval to be rejected", role)
		}
	}
	if err := mgr.ApprovePromotion(context.Background(), control.Requester{Human: true}, "%2"); err != nil {
		t.Fatalf("ApprovePromotion: %v", err)
	}
	byID := make(map[string]TrackedPane)
	for _, pane := range mgr.ListPanes() {
		byID[pane.PaneID] = pane
	}
	promoted := byID["%2"]
	if promoted.Role != control.RoleManager || promoted.ParentID != "%0" || promoted.RootID != "%0" || promoted.PendingPromotion != "" {
		t.Fatalf("unexpected promoted pane: %+v", promoted)
	}
	if promoted.WindowName != "manager" {
		t.Fatalf("promoted window = %q, want manager", promoted.WindowName)
	}
	if mock.findCall("join-pane") == nil {
		t.Fatal("expected worker to move to its former manager's window")
	}
	for _, call := range mock.findCalls("send-keys") {
		if strings.Contains(strings.Join(call, " "), "/handoff") {
			t.Fatal("promotion must not inject commands into the worker's terminal")
		}
	}
	if err := mgr.ApprovePromotion(context.Background(), control.Requester{Human: true}, "%2"); err == nil {
		t.Fatal("manager-to-controller promotion must be rejected")
	}
}

func TestCapabilitiesUseConfiguredShortcut(t *testing.T) {
	mgr := newTestManager(&testMock{})
	mgr.cfg.Keys.Prefix = "C-Space"
	if got := mgr.Capabilities(); got.Protocol != 1 || got.PromotionShortcut != "Ctrl+Space then Shift+P" {
		t.Fatalf("capabilities = %+v", got)
	}
	mgr.cfg.Keys.Prefix = "C-a"
	mgr.cfg.Keys.ApprovePromotion = "p"
	if got := mgr.Capabilities().PromotionShortcut; got != "Ctrl+a then p" {
		t.Fatalf("shortcut = %q", got)
	}
}

func TestLegacyRoleMigration(t *testing.T) {
	mock := &testMock{listOutput: "1\tcontrol\t%4\t0\tpi\t/tmp\t1\t104\tmanager\t\t\t\n2\twork\t%7\t0\tpi\t/tmp\t1\t107\tmanager\t\t\t\n2\twork\t%8\t1\tpi\t/tmp\t0\t108\tworker\t%7\t%7\t\n1\tcontrol\t%9\t1\tpi\t/tmp\t0\t109\tworker\t%4\t%4\t"}
	mgr := newTestManager(mock)
	if err := mgr.AdoptOrphans(context.Background()); err == nil {
		t.Fatal("legacy roles require explicit migration")
	}
	if len(mock.findCalls("set-option")) != 0 {
		t.Fatal("refused adoption must not change pane options")
	}
	if err := mgr.MigrateLegacyRoles(context.Background(), "%4"); err != nil {
		t.Fatal(err)
	}
	options := make(map[string]map[string]string)
	for _, call := range mock.findCalls("set-option") {
		if options[call[3]] == nil {
			options[call[3]] = make(map[string]string)
		}
		options[call[3]][call[4]] = call[5]
	}
	for paneID, role := range map[string]string{"%4": "controller", "%7": "manager", "%8": "worker", "%9": "worker"} {
		if options[paneID]["@agency_role"] != role || options[paneID]["@agency_root"] != "%4" {
			t.Fatalf("pane %s options = %v", paneID, options[paneID])
		}
	}
	if options["%7"]["@agency_parent"] != "%4" || options["%8"]["@agency_parent"] != "%7" || options["%9"]["@agency_parent"] != "%4" {
		t.Fatalf("unexpected lineage: %v", options)
	}
}

func TestLegacySelfParentMigration(t *testing.T) {
	mock := &testMock{listOutput: "1\tcontrol\t%4\t0\tpi\t/tmp\t1\t104\tmanager\t%4\t%4\t\n2\twork\t%7\t0\tpi\t/tmp\t1\t107\tmanager\t%4\t%4\t\n2\twork\t%8\t1\tpi\t/tmp\t0\t108\tworker\t%7\t%7\t"}
	mgr := newTestManager(mock)
	if err := mgr.AdoptOrphans(context.Background()); err == nil {
		t.Fatal("self-parented legacy root requires explicit migration")
	}
	if len(mock.findCalls("set-option")) != 0 {
		t.Fatal("refused adoption must not change pane options")
	}
	if err := mgr.MigrateLegacyRoles(context.Background(), "%4"); err != nil {
		t.Fatal(err)
	}
	options := make(map[string]string)
	for _, call := range mock.findCalls("set-option") {
		options[call[3]+call[4]] = call[5]
	}
	for key, want := range map[string]string{
		"%4@agency_role": "controller", "%4@agency_parent": "", "%4@agency_root": "%4",
		"%7@agency_role": "manager", "%7@agency_parent": "%4", "%7@agency_root": "%4",
		"%8@agency_role": "worker", "%8@agency_parent": "%7", "%8@agency_root": "%4",
	} {
		if got, ok := options[key]; !ok || got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestLegacyMigrationRejectsInvalidController(t *testing.T) {
	for _, controllerID := range []string{"%2", "%3", "%4", "%99"} {
		mock := &testMock{listOutput: "1\tcontrol\t%1\t0\tpi\t/tmp\t1\t101\tmanager\t\t\t\n1\tcontrol\t%2\t1\tpi\t/tmp\t0\t102\tworker\t%1\t%1\t\n2\twork\t%3\t0\tpi\t/tmp\t1\t103\tmanager\t%1\t%1\t\n2\twork\t%4\t1\tpi\t/tmp\t0\t104\tworker\t%4\t%4\t"}
		mgr := newTestManager(mock)
		if err := mgr.MigrateLegacyRoles(context.Background(), controllerID); err == nil {
			t.Fatalf("controller %s must be rejected", controllerID)
		}
		if len(mock.findCalls("set-option")) != 0 {
			t.Fatal("invalid migration must not change pane options")
		}
	}
}

func TestLegacyWorkerPromotion(t *testing.T) {
	mock := &testMock{listOutput: "1\tcontrol\t%4\t0\tpi\t/tmp\t1\t104\tcontroller\t\t%4\t\n1\tcontrol\t%9\t1\tpi\t/tmp\t0\t109\tworker\t%4\t%4\t"}
	mgr := newTestManager(mock)
	if err := mgr.AdoptOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	worker := control.Requester{PaneID: "%9", Role: control.RoleWorker, RootID: "%4"}
	if err := mgr.RequestPromotion(context.Background(), worker, "coordinate review"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ApprovePromotion(context.Background(), control.Requester{Human: true}, "%9"); err != nil {
		t.Fatal(err)
	}
	for _, pane := range mgr.ListPanes() {
		if pane.PaneID == "%9" && (pane.Role != control.RoleManager || pane.ParentID != "%4") {
			t.Fatalf("unexpected promotion: %+v", pane)
		}
	}
}

func TestAdoptOrphansRestoresPendingPromotion(t *testing.T) {
	mock := &testMock{listOutput: "1\tmain\t%0\t0\tpi\t/tmp\t1\t100\tcontroller\t\t%0\t\t\n1\tmain\t%2\t1\tpi\t/tmp\t0\t102\tworker\t%1\t%0\tbmVlZCBmYW5vdXQ\tZWNobyBoaQ"}
	mgr := newTestManager(mock)
	if err := mgr.AdoptOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, pane := range mgr.ListPanes() {
		if pane.PaneID == "%2" && (pane.PendingPromotion != "need fanout" || pane.Command != "echo hi") {
			t.Fatalf("restored pane = %+v", pane)
		}
	}
}

func TestKillPane(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	_ = mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "")
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

func TestKillWindow(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	_ = mgr.SpawnAgentWindow(context.Background(), testController, control.RoleManager, "casts-review", "claude", "")
	if mgr.PaneCount() != 1 {
		t.Fatalf("expected 1 pane, got %d", mgr.PaneCount())
	}

	if err := mgr.KillWindow(context.Background(), "casts-review"); err != nil {
		t.Fatalf("KillWindow failed: %v", err)
	}
	if mgr.PaneCount() != 0 {
		t.Errorf("expected 0 panes after kill, got %d", mgr.PaneCount())
	}
}

func TestRenameWindow(t *testing.T) {
	mock := &testMock{windowOutput: "@1\t1\tcasts-review"}
	mgr := newTestManager(mock)

	_ = mgr.SpawnAgentWindow(context.Background(), testController, control.RoleManager, "casts-review", "claude", "")
	mock.listOutput = "1\thammerbound\t%1\t0\tpi\t/tmp\t1\t123"
	if err := mgr.RenameWindow(context.Background(), "casts-review", "hammerbound"); err != nil {
		t.Fatalf("RenameWindow failed: %v", err)
	}
	panes := mgr.ListPanes()
	if panes[0].WindowName != "hammerbound" {
		t.Errorf("expected window name hammerbound, got %q", panes[0].WindowName)
	}
	renameCall := mock.findCall("rename-window")
	if renameCall == nil || renameCall[2] != "@1" || renameCall[3] != "hammerbound" {
		t.Errorf("unexpected rename-window call: %v", renameCall)
	}
}

func TestSendTextAttribution(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	requester := control.Requester{PaneID: "%7", Role: control.RoleWorker, RootID: "%0"}
	if err := mgr.SendText(context.Background(), requester, "%8", "give test steps", true); err != nil {
		t.Fatalf("SendText failed: %v", err)
	}
	call := mock.findCall("send-keys")
	if call == nil {
		t.Fatal("expected a send-keys call")
	}
	text := call[len(call)-1]
	if !strings.HasPrefix(text, "[from %7 worker] give test steps") {
		t.Errorf("expected attribution prefix, got %q", text)
	}
}

func TestKillAll(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	_ = mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "")
	_ = mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "codex", "")
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

func TestResolveRequesterFromProcessAncestry(t *testing.T) {
	mock := &testMock{listOutput: "1\tjournalia\t%0\t0\tpi\t/tmp\t1\t100\tcontroller\t\t%0"}
	mgr := newTestManager(mock)
	mgr.processOwnedBy = func(pid, ancestor int) bool { return pid == 200 && ancestor == 100 }
	if err := mgr.AdoptOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	req, err := mgr.ResolveRequester(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}
	if req.PaneID != "%0" || req.Role != control.RoleController {
		t.Fatalf("unexpected requester: %+v", req)
	}
	if _, err := mgr.ResolveRequester(context.Background(), 201); err == nil {
		t.Fatal("expected unknown process to be rejected")
	}
}

func TestPruneDead(t *testing.T) {
	mock := &testMock{}
	mgr := newTestManager(mock)

	if err := mgr.SpawnAgent(context.Background(), testController, control.RoleWorker, "claude", ""); err != nil {
		t.Fatalf("SpawnAgent failed: %v", err)
	}
	if err := mgr.SpawnAgent(context.Background(), testController, control.RoleWorker, "codex", ""); err != nil {
		t.Fatalf("SpawnAgent failed: %v", err)
	}
	if mgr.PaneCount() != 2 {
		t.Fatalf("expected 2 panes, got %d", mgr.PaneCount())
	}

	// Only %1 is still alive in tmux; %2 died outside agency.
	mock.listOutput = "1\ttest\t%1\t0\tclaude\t/tmp\t1\t123\tworker\t%0\t%0"

	removed, err := mgr.PruneDead(context.Background())
	if err != nil {
		t.Fatalf("PruneDead failed: %v", err)
	}
	if len(removed) != 1 || removed[0] != "%2" {
		t.Fatalf("expected [%%2] pruned, got %v", removed)
	}
	if mgr.PaneCount() != 1 {
		t.Fatalf("expected 1 pane after prune, got %d", mgr.PaneCount())
	}

	// Pruning again is a no-op.
	removed, err = mgr.PruneDead(context.Background())
	if err != nil {
		t.Fatalf("PruneDead failed: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("expected no panes pruned, got %v", removed)
	}
}

func TestResolveRequesterFromTmuxServer(t *testing.T) {
	mock := &testMock{listOutput: "1\tjournalia\t%0\t0\tpi\t/tmp\t1\t100\tcontroller\t\t%0"}
	mgr := newTestManager(mock)
	// pid 300 descends from the tmux server (42), not from any pane.
	mgr.processOwnedBy = func(pid, ancestor int) bool { return pid == 300 && ancestor == 42 }
	if err := mgr.AdoptOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	req, err := mgr.ResolveRequester(context.Background(), 300)
	if err != nil {
		t.Fatalf("keybinding process should resolve as controller: %v", err)
	}
	if req.PaneID != "%0" || req.Role != control.RoleController {
		t.Fatalf("unexpected requester: %+v", req)
	}
}

func TestResolveRequesterFromTmuxServerWithoutControllerPane(t *testing.T) {
	// Only a manager pane is alive; the controller pane was closed.
	mock := &testMock{listOutput: "1\tjournalia\t%1\t0\tpi\t/tmp\t1\t100\tmanager\t%0\t%0"}
	mgr := newTestManager(mock)
	mgr.processOwnedBy = func(pid, ancestor int) bool { return pid == 300 && ancestor == 42 }
	if err := mgr.AdoptOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	req, err := mgr.ResolveRequester(context.Background(), 300)
	if err != nil {
		t.Fatalf("keybinding process should resolve as controller: %v", err)
	}
	if req.PaneID != "" || req.Role != control.RoleController {
		t.Fatalf("unexpected requester: %+v", req)
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

	if err := mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "/home/user/myproject"); err != nil {
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
	_ = mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "/proj/a")
	_ = mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "/proj/b")
	_ = mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "/proj/c")

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

	_ = mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "")
	_ = mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "")
	_ = mgr.SpawnAgent(context.Background(), testController, control.RoleManager, "claude", "")

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
