package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lemonsaurus/agency/internal/agents"
	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/ipc"
	"github.com/lemonsaurus/agency/internal/palette"
	"github.com/lemonsaurus/agency/internal/session"
	"github.com/lemonsaurus/agency/internal/status"
	"github.com/lemonsaurus/agency/internal/tmux"
)

func main() {
	if len(os.Args) < 2 {
		runLaunch("")
		return
	}

	switch os.Args[1] {
	case "--migrate-controller":
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "Usage: agency --migrate-controller <pane-id>")
			os.Exit(1)
		}
		runLaunch(os.Args[2])
	case "serve":
		runServe()
	case "cloud":
		runCloud(os.Args[2:])
	case "cloud-view":
		runCloudView(os.Args[2:])
	case "sync-cloud":
		runSyncCloud()
	case "cloud-act":
		runCloudAct(os.Args[2:])
	case "spawn":
		runSpawn(os.Args[2:])
	case "spawn-dialog":
		runSpawnDialog(os.Args[2:])
	case "whoami":
		runWhoAmI(os.Args[2:])
	case "capabilities":
		runCapabilities()
	case "replace":
		runReplace(os.Args[2:])
	case "request-promotion":
		runRequestPromotion(os.Args[2:])
	case "approve-promotion":
		runApprovePromotion(os.Args[2:])
	case "send":
		runSend(os.Args[2:])
	case "capture":
		runCapture(os.Args[2:])
	case "kill":
		runKill(os.Args[2:])
	case "move":
		runMove(os.Args[2:])
	case "rename-window":
		runRenameWindow(os.Args[2:])
	case "kill-all":
		runKillAll()
	case "list":
		runList(os.Args[2:])
	case "layout":
		runLayout(os.Args[2:])
	case "relayout":
		runRelayout()
	case "broadcast-dialog":
		runBroadcastDialog()
	case "broadcast-keys":
		runBroadcastKeys(os.Args[2:])
	case "attach":
		runAttach()
	case "config":
		runConfig()
	case "palette":
		runPalette()
	case "logs":
		runLogs()
	case "debug-mouse":
		runDebugMouse(os.Args[2:])
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`agency — AI agent session manager for tmux

Usage:
  agency                            Launch new session (or reattach)
  agency --migrate-controller <pane> Migrate a legacy session under the selected controller
  agency serve                      Run the daemon headless on its own tmux server (no attach)
  agency cloud <command> ...        Run a command against the headless server (used over SSH)
  agency cloud attach <window-id>   Attach this terminal to one headless window
  agency sync-cloud                 Mirror the cloud host's panes into the cloud-harness window
  agency cloud-view <window-id>     Viewer pane process: attach and reconnect (used by sync-cloud)
  agency spawn <agent> [dir...]     Spawn agent pane(s), one per dir (claude, codex, ...)
  agency spawn --cmd "..." [dir]    Spawn arbitrary command
  agency spawn --window <name> ...   Spawn into a named tmux window
  agency spawn --role <role> ...     Request a manager or worker pane
  agency spawn-dialog <agent> [dir] Open directory picker popup, then spawn
  agency whoami [--role]            Print current pane authority
  agency capabilities              Print running daemon protocol and promotion shortcut
  agency replace [--cmd "..." dir] Start a role-preserving successor pane
  agency request-promotion <reason> Request worker promotion
  agency approve-promotion <pane>   Approve a pending worker (tmux keybinding only)
  agency send <pane-id> <text>      Send text to a pane and press Enter
  agency capture <pane-id> [lines]  Capture pane output
  agency kill <pane-id>             Kill a specific pane
  agency kill --window <name>       Kill a specific window
  agency move <pane-id> <window>   Move a pane into a window (created if missing)
  agency rename-window <target> <name> Rename a window by name, index, or id
  agency kill-all                   Kill all agent panes
  agency list                       List all panes with status
  agency layout <layout>            Switch layout (tiled, columns, rows, main-vertical)
  agency attach                     Reattach to existing session
  agency config                     Print resolved config
  agency palette                    Open command palette (used by tmux keybinding)
  agency logs                       Print path to log file (tail -f it)
  agency debug-mouse [off]          Log mouse click events to a file (diagnose dropped clicks)
  agency help                       Show this help`)
}

func loadConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Loading config: %v", err)
	}
	return cfg
}

func socketPath(sessionName string) string {
	return fmt.Sprintf("/tmp/agency-%s.sock", sessionName)
}

func lockPath(sessionName string) string {
	return fmt.Sprintf("/tmp/agency-%s.lock", sessionName)
}

func agencyBinPath() string {
	bin, err := os.Executable()
	if err != nil {
		return "agency"
	}
	return bin
}

// acquireLock tries to get an exclusive flock. Returns the file (keep open) or error.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("another agency instance is running — try: agency attach")
	}
	return f, nil
}

func logPath(sessionName string) string {
	return fmt.Sprintf("/tmp/agency-%s.log", sessionName)
}

// setupLogging redirects log output to a file and prints the path to stderr.
func setupLogging(sessionName string) {
	path := logPath(sessionName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not open log file %s: %v\n", path, err)
		return
	}
	log.SetOutput(f)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	fmt.Fprintf(os.Stderr, "agency: logging to %s\n", path)
}

// daemon is a running Agency backend: tmux session, manager, IPC, poller.
type daemon struct {
	ctx    context.Context
	cancel context.CancelFunc
	tc     *tmux.Client
	close  func()
}

func runLaunch(controllerPane string) {
	d := startDaemon(loadConfig(), false, controllerPane)
	defer d.close()

	// Attach to tmux (this blocks until detach or session end).
	log.Printf("Attaching to tmux session %q...", d.tc.SessionName)
	if err := d.tc.Attach(d.ctx); err != nil {
		// Attach failing is normal on detach.
		if d.ctx.Err() == nil {
			log.Printf("Detached from tmux session.")
		}
	}

	d.cancel()
	log.Println("Shutting down.")
}

// runServe keeps the daemon alive without a terminal, on a tmux server named
// after the session so it never collides with a local interactive Agency.
func runServe() {
	cfg := loadConfig()
	d := startDaemon(cfg, true, "")
	defer d.close()

	log.Printf("Serving session %q headless.", cfg.Session.Name)
	<-d.ctx.Done()
	log.Println("Shutting down.")
}

// runCloud points the ordinary CLI at the headless server. The IPC socket is
// shared by session name already; only direct tmux calls need the server name.
func runCloud(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud <command> ...")
		os.Exit(1)
	}
	cfg := loadConfig()
	os.Setenv("AGENCY_TMUX_SOCKET", "agency-"+cfg.Session.Name)
	if args[0] == "attach" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "Usage: agency cloud attach <window-id>")
			os.Exit(1)
		}
		tc := tmux.NewClient(cfg.Session.Name, "")
		if err := tc.AttachWindow(context.Background(), args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "Error attaching: %v\n", err)
			os.Exit(1)
		}
		return
	}
	os.Args = append([]string{os.Args[0]}, args...)
	main()
}

func runSyncCloud() {
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "sync-cloud")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if strings.HasPrefix(resp, "error:") {
		fmt.Fprintln(os.Stderr, resp)
		os.Exit(1)
	}
	fmt.Println(resp)
}

// runCloudAct is what keybindings and menus call when the focused pane is a
// viewer: act on the remote agent behind it, then re-sync the mirror.
//
//	cloud-act spawn <window> <agent>|--cmd <command>   new remote agent in that pane's directory
//	cloud-act kill <window>                            kill the remote agent
//	cloud-act approve <window>                         approve its pending promotion
func runCloudAct(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud-act spawn|kill|approve <window-id> ...")
		os.Exit(1)
	}
	cfg := loadConfig()
	remote := &cloud.Client{Host: cfg.Cloud.Host}
	ctx := context.Background()
	windows, err := remote.Windows(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	var target *cloud.Window
	for i := range windows {
		if windows[i].ID == args[1] {
			target = &windows[i]
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "Error: remote window %s not found\n", args[1])
		os.Exit(1)
	}
	var remoteArgs []string
	switch args[0] {
	case "spawn":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: agency cloud-act spawn <window-id> <agent>|--cmd <command>")
			os.Exit(1)
		}
		remoteArgs = append([]string{"spawn"}, args[2:]...)
		remoteArgs = append(remoteArgs, target.Pane.CWD)
	case "kill":
		remoteArgs = []string{"kill", target.Pane.ID}
	case "approve":
		remoteArgs = []string{"approve-promotion", target.Pane.ID}
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown cloud action %s\n", args[0])
		os.Exit(1)
	}
	if _, err := remote.Run(ctx, 20*time.Second, remoteArgs...); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	runSyncCloud()
}

// runCloudView is the viewer pane's process: it stays attached to one remote
// window, reconnects after a dropped link, and exits once the window is gone.
// It never restarts the agent. With no window id it is the offline
// placeholder and retries the sync on Enter.
func runCloudView(args []string) {
	cfg := loadConfig()
	remote := &cloud.Client{Host: cfg.Cloud.Host}
	ctx := context.Background()
	if len(args) == 0 || args[0] == "offline" {
		for {
			fmt.Printf("☁  %s unreachable. Press Enter to retry.\n", cfg.Cloud.Host)
			fmt.Scanln()
			if resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "sync-cloud"); err == nil && !strings.HasPrefix(resp, "error:") {
				return
			}
		}
	}
	windowID := args[0]
	delay := time.Second
	for {
		_ = remote.Attach(ctx, windowID)
		windows, err := remote.Windows(ctx)
		if err != nil {
			fmt.Printf("\n☁  link dropped (%v). Reconnecting in %s...\n", err, delay)
			time.Sleep(delay)
			if delay < 30*time.Second {
				delay *= 2
			}
			continue
		}
		delay = time.Second
		if !hasWindow(windows, windowID) {
			fmt.Printf("\n☁  remote pane %s is gone.\n", windowID)
			return
		}
	}
}

func hasWindow(windows []cloud.Window, id string) bool {
	for _, window := range windows {
		if window.ID == id {
			return true
		}
	}
	return false
}

// startDaemon brings up the backend. cloud selects the headless profile: a
// tmux server named agency-<session>, the chrome-free cloud.conf, and one
// window per pane.
func startDaemon(cfg *config.Config, cloud bool, controllerPane string) *daemon {
	sessionName := cfg.Session.Name

	// Set up file logging before anything else.
	setupLogging(sessionName)

	// Acquire lock.
	lockFile, err := acquireLock(lockPath(sessionName))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Generate tmux config.
	var confPath string
	if cloud {
		confPath, err = tmux.GenerateCloudConfig()
	} else {
		confPath, err = tmux.GenerateConfig(cfg, agencyBinPath())
	}
	if err != nil {
		log.Fatalf("Generating tmux config: %v", err)
	}

	// Create tmux client.
	tc := tmux.NewClient(sessionName, confPath)
	tmuxSocket := ""
	if cloud {
		tmuxSocket = "agency-" + sessionName
		tc.Cmd = &tmux.ExecCommander{SocketName: tmuxSocket}
	}

	// Set up signal handling with double Ctrl+C.
	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	signal.Ignore(syscall.SIGHUP)

	go func() {
		<-sigCh // first signal
		cancel()
		<-sigCh // second signal
		os.Exit(1)
	}()

	// Create agent registry.
	registry := agents.NewRegistry(cfg.Agents, cfg.AgentOrder)

	// Create status poller. Status changes light up window names for
	// windows containing panes that wait for input.
	attention := &attentionTracker{tc: tc, last: make(map[int]bool)}
	var poller *status.Poller
	poller = status.NewPoller(tc, func(paneID, agentType, s string) {
		log.Printf("status: %s (%s) → %s", paneID, agentType, s)
		attention.update(ctx, poller.Snapshot())
	})

	// Create session manager.
	mgr := session.NewManager(tc, registry, cfg, poller)
	mgr.WindowPerPane = cloud
	mgr.OutsideIsHuman = cloud

	// Check if tmux session already exists (crash recovery).
	if tc.SessionExists(ctx) {
		if err := tc.SourceConfig(ctx); err != nil {
			log.Printf("Warning: reloading tmux config: %v", err)
		}
		log.Printf("Existing tmux session found, adopting orphan panes...")
	} else {
		if err := tc.NewSession(ctx); err != nil {
			log.Fatalf("Creating tmux session: %v", err)
		}
	}
	if err := tc.NameFirstWindow(ctx, "control"); err != nil {
		log.Printf("Warning: naming control window: %v", err)
	}
	if controllerPane != "" {
		if err := mgr.MigrateLegacyRoles(ctx, controllerPane); err != nil {
			log.Fatalf("Migrating legacy roles: %v", err)
		}
	}
	// Label all existing panes (initial shell on fresh start, or orphans on recovery).
	if err := mgr.AdoptOrphans(ctx); err != nil {
		log.Fatalf("Adopting panes: %v", err)
	}

	// Start IPC socket server.
	sockPath := socketPath(sessionName)
	srv := ipc.NewServer(sockPath, mgr)
	if err := srv.Start(); err != nil {
		log.Fatalf("Starting socket server: %v", err)
	}

	// Set AGENCY_SOCKET in the tmux session environment.
	if err := tc.SetEnv(ctx, "AGENCY_SOCKET", sockPath); err != nil {
		log.Printf("Warning: setting AGENCY_SOCKET: %v", err)
	}
	if tmuxSocket != "" {
		if err := tc.SetEnv(ctx, "AGENCY_TMUX_SOCKET", tmuxSocket); err != nil {
			log.Printf("Warning: setting AGENCY_TMUX_SOCKET: %v", err)
		}
	}

	// Start status poller.
	go poller.Run(ctx)

	// Prune panes that die outside agency (crashes, /quit) so the poller
	// stops polling them and spawn limits stay accurate.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				removed, err := mgr.PruneDead(ctx)
				if err != nil {
					continue
				}
				for _, id := range removed {
					log.Printf("pruned dead pane %s", id)
				}
				if len(removed) > 0 {
					attention.update(ctx, poller.Snapshot())
				}
			}
		}
	}()

	// Mirror the cloud host without blocking startup.
	if !cloud && cfg.Cloud.Host != "" {
		go func() {
			if result, err := mgr.SyncCloud(ctx); err != nil {
				log.Printf("sync-cloud: %v", err)
			} else {
				log.Printf("sync-cloud: %s", result)
			}
		}()
	}

	return &daemon{ctx: ctx, cancel: cancel, tc: tc, close: func() {
		cancel()
		srv.Close()
		lockFile.Close()
		os.Remove(lockPath(sessionName))
	}}
}

// attentionTracker mirrors waiting-pane state onto @agency_attention window
// options so the status bar can highlight windows that need input.
type attentionTracker struct {
	tc   *tmux.Client
	mu   sync.Mutex
	last map[int]bool // window index → attention currently set
}

func (a *attentionTracker) update(ctx context.Context, statuses map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	panes, err := a.tc.ListPanes(ctx)
	if err != nil {
		return
	}
	waiting := make(map[int]bool)
	sample := make(map[int]string) // window index → any pane ID in it
	for _, p := range panes {
		if _, ok := sample[p.WindowIndex]; !ok {
			sample[p.WindowIndex] = p.ID
		}
		if statuses[p.ID] == status.StatusWaiting {
			waiting[p.WindowIndex] = true
		}
	}
	for idx, paneID := range sample {
		if a.last[idx] == waiting[idx] {
			continue
		}
		val := ""
		if waiting[idx] {
			val = "1"
		}
		if err := a.tc.SetWindowOption(ctx, paneID, "@agency_attention", val); err == nil {
			a.last[idx] = waiting[idx]
		}
	}
}

func runSpawn(args []string) {
	cfg := loadConfig()
	sockPath := socketPath(cfg.Session.Name)

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency spawn [--window name] [--role manager|worker] <agent|--cmd> [dir]")
		os.Exit(1)
	}

	windowName := ""
	role := ""
	for len(args) > 0 && strings.HasPrefix(args[0], "--") && args[0] != "--cmd" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Error: spawn option needs a value")
			os.Exit(1)
		}
		switch args[0] {
		case "--window":
			windowName = args[1]
		case "--role":
			parsed, err := control.ParseRole(args[1])
			if err != nil || parsed == control.RoleController {
				fmt.Fprintln(os.Stderr, "Error: role must be manager or worker")
				os.Exit(1)
			}
			role = string(parsed)
		default:
			fmt.Fprintf(os.Stderr, "Error: unknown spawn option %s\n", args[0])
			os.Exit(1)
		}
		args = args[2:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Error: agent or command is required")
		os.Exit(1)
	}

	var msgs []string
	if args[0] == "--cmd" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: agency spawn --cmd \"command\" [dir]")
			os.Exit(1)
		}
		command, dir := extractDirArg(args[1:])
		msgs = []string{spawnCommandMessage(windowName, role, strings.Join(command, " "), dir)}
	} else {
		name := args[0]
		dirs := args[1:]
		if len(dirs) == 0 {
			dirs = []string{currentDir()}
		}
		for _, dir := range dirs {
			abs, ok := resolveDir(dir)
			if !ok {
				continue
			}
			msgs = append(msgs, spawnAgentMessage(windowName, role, name, abs))
		}
		if len(msgs) == 0 {
			fmt.Fprintln(os.Stderr, "Error: no valid directories to spawn in")
			os.Exit(1)
		}
	}

	for _, msg := range msgs {
		resp, err := ipc.SendMessage(sockPath, msg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\nIs agency running? Try: agency\n", err)
			os.Exit(1)
		}
		if resp != "ok" {
			fmt.Fprintf(os.Stderr, "Error: %s\n", resp)
			os.Exit(1)
		}
	}
}

func runWhoAmI(args []string) {
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "whoami")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if strings.HasPrefix(resp, "error:") {
		fmt.Fprintln(os.Stderr, resp)
		os.Exit(1)
	}
	if len(args) > 0 && args[0] == "--role" {
		var requester control.Requester
		if err := json.Unmarshal([]byte(resp), &requester); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(requester.Role)
		return
	}
	fmt.Println(resp)
}

func runCapabilities() {
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "capabilities")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Agency daemon unavailable: %v\n", err)
		os.Exit(1)
	}
	var capabilities control.Capabilities
	if err := json.Unmarshal([]byte(resp), &capabilities); err != nil || capabilities.Protocol != 1 {
		fmt.Fprintln(os.Stderr, "Agency daemon is outdated. Relaunch Agency without killing the tmux session.")
		os.Exit(1)
	}
	fmt.Println(resp)
}

func runReplace(args []string) {
	message := "replace"
	if len(args) > 0 {
		if args[0] != "--cmd" || len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: agency replace [--cmd \"command\" dir]")
			os.Exit(1)
		}
		command, dir := extractDirArg(args[1:])
		payload, _ := json.Marshal(struct {
			Command string `json:"command,omitempty"`
			Dir     string `json:"dir,omitempty"`
		}{Command: strings.Join(command, " "), Dir: dir})
		message += ":" + string(payload)
	}
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), message)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if strings.HasPrefix(resp, "error:") {
		fmt.Fprintln(os.Stderr, resp)
		os.Exit(1)
	}
	fmt.Println(resp)
}

func runRequestPromotion(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency request-promotion <reason>")
		os.Exit(1)
	}
	payload, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: strings.Join(args, " ")})
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "promotion-request:"+string(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if resp != "ok" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp)
		os.Exit(1)
	}
}

func runApprovePromotion(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: agency approve-promotion <pane-id>")
		os.Exit(1)
	}
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "promotion-approve:"+args[0])
	message := fmt.Sprintf("Pane %s promoted to manager. Pi refreshes tools on the next prompt.", args[0])
	if err != nil {
		message = fmt.Sprintf("Promotion failed: %v", err)
	} else if resp != "ok" {
		message = "Promotion failed: " + resp
	}
	tc := tmux.NewClient(cfg.Session.Name, "")
	_ = tc.DisplayMessage(context.Background(), args[0], message)
	if err != nil || resp != "ok" {
		fmt.Fprintln(os.Stderr, message)
		os.Exit(1)
	}
}

func runSpawnDialog(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency spawn-dialog <agent> [default-dir]")
		os.Exit(1)
	}
	agentName := args[0]
	// Popups opened from a viewer pane carry the remote window; the directory
	// is the remote pane's, so there is nothing to pick locally.
	if window := os.Getenv("AGENCY_CLOUD_WINDOW"); window != "" {
		runCloudAct([]string{"spawn", window, agentName})
		return
	}
	defaultDir := ""
	if len(args) >= 2 {
		defaultDir = args[1]
	}
	if defaultDir == "" {
		defaultDir = currentDir()
	}
	bin := agencyBinPath()
	if err := palette.RunSpawnDialog(agentName, bin, defaultDir); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

type spawnPayload struct {
	Window  string `json:"window,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Command string `json:"command,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Role    string `json:"role,omitempty"`
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

type renameWindowPayload struct {
	Target string `json:"target"`
	Name   string `json:"name"`
}

func spawnAgentMessage(windowName, role, name, dir string) string {
	if windowName == "" && role == "" {
		return "spawn:" + name + dirSuffix(dir)
	}
	payload, _ := json.Marshal(spawnPayload{Window: windowName, Agent: name, Dir: dir, Role: role})
	if windowName == "" {
		return "spawn-role:" + string(payload)
	}
	return "spawn-window:" + string(payload)
}

func spawnCommandMessage(windowName, role, command, dir string) string {
	if windowName == "" && role == "" {
		return "spawn:cmd:" + command + dirSuffix(dir)
	}
	payload, _ := json.Marshal(spawnPayload{Window: windowName, Command: command, Dir: dir, Role: role})
	if windowName == "" {
		return "spawn-role:" + string(payload)
	}
	return "spawn-window:" + string(payload)
}

// dirSuffix returns the @/path IPC suffix for a directory, or empty string.
func dirSuffix(dir string) string {
	if dir == "" {
		return ""
	}
	return "@" + dir
}

// currentDir returns the working directory, or empty string on error.
func currentDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

// resolveDir resolves a path to an absolute directory path.
// Returns the absolute path and true, or warns and returns false if the path
// is not a directory or doesn't exist.
func resolveDir(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: skipping %q: %v\n", path, err)
		return "", false
	}
	info, err := os.Stat(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: skipping %q: %v\n", path, err)
		return "", false
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Warning: skipping %q: not a directory\n", path)
		return "", false
	}
	return abs, true
}

// extractDirArg checks if the last argument in a --cmd invocation is an absolute
// path (the directory). Returns the command args and the dir separately.
func extractDirArg(args []string) (command []string, dir string) {
	if len(args) >= 2 && strings.HasPrefix(args[len(args)-1], "/") {
		return args[:len(args)-1], args[len(args)-1]
	}
	return args, currentDir()
}

func runSend(args []string) {
	enter := true
	if len(args) > 0 && args[0] == "--no-enter" {
		enter = false
		args = args[1:]
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: agency send [--no-enter] <pane-id> <text>")
		os.Exit(1)
	}
	payload, _ := json.Marshal(sendPayload{Pane: args[0], Text: strings.Join(args[1:], " "), Enter: enter})
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "send:"+string(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if resp != "ok" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp)
		os.Exit(1)
	}
}

func runCapture(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency capture <pane-id> [lines]")
		os.Exit(1)
	}
	lines := 200
	if len(args) >= 2 {
		if _, err := fmt.Sscanf(args[1], "%d", &lines); err != nil || lines <= 0 {
			fmt.Fprintln(os.Stderr, "Error: lines must be a positive integer")
			os.Exit(1)
		}
	}
	cfg := loadConfig()
	tc := tmux.NewClient(cfg.Session.Name, "")
	out, err := tc.CapturePaneContent(context.Background(), args[0], lines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(out)
}

func runKill(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency kill <pane-id> or agency kill --window <name>")
		os.Exit(1)
	}
	cfg := loadConfig()
	message := "kill:" + args[0]
	if args[0] == "--window" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: agency kill --window <name>")
			os.Exit(1)
		}
		if agencyRole() == "worker" {
			fmt.Fprintln(os.Stderr, "Error: worker panes cannot kill windows")
			os.Exit(1)
		}
		message = "kill-window:" + args[1]
	} else if agencyRole() == "worker" && os.Getenv("AGENCY_PANE_ID") != args[0] {
		fmt.Fprintln(os.Stderr, "Error: worker panes may only kill their own pane")
		os.Exit(1)
	}
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), message)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if resp != "ok" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp)
		os.Exit(1)
	}
}

func runMove(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: agency move <pane-id> <window-name>")
		os.Exit(1)
	}
	if agencyRole() == "worker" && os.Getenv("AGENCY_PANE_ID") != args[0] {
		fmt.Fprintln(os.Stderr, "Error: worker panes may only move their own pane")
		os.Exit(1)
	}
	window := strings.Join(args[1:], " ")
	payload, _ := json.Marshal(movePayload{Pane: args[0], Window: window})
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "move:"+string(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if resp != "ok" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp)
		os.Exit(1)
	}
	fmt.Printf("moved %s to window %s\n", args[0], window)
}

func runRenameWindow(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: agency rename-window <target> <name>")
		os.Exit(1)
	}
	payload, _ := json.Marshal(renameWindowPayload{Target: args[0], Name: strings.Join(args[1:], " ")})
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "rename-window:"+string(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if resp != "ok" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp)
		os.Exit(1)
	}
	fmt.Printf("renamed window %s to %s\n", args[0], strings.Join(args[1:], " "))
}

func runKillAll() {
	if agencyRole() == "worker" {
		fmt.Fprintln(os.Stderr, "Error: worker panes cannot kill all panes")
		os.Exit(1)
	}
	cfg := loadConfig()
	// Kill all is done via the session manager. For now, use tmux directly.
	tc := tmux.NewClient(cfg.Session.Name, "")
	ctx := context.Background()
	panes, err := tc.ListPanes(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing panes: %v\n", err)
		os.Exit(1)
	}
	for _, pane := range panes {
		resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "kill:"+pane.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error killing %s: %v\n", pane.ID, err)
		} else if resp != "ok" {
			fmt.Fprintf(os.Stderr, "Error killing %s: %s\n", pane.ID, resp)
		}
	}
}

func agencyRole() string {
	if role := os.Getenv("AGENCY_ROLE"); role != "" {
		return role
	}
	return "manager"
}

func runList(args []string) {
	cfg := loadConfig()
	tc := tmux.NewClient(cfg.Session.Name, "")
	ctx := context.Background()
	panes, err := tc.ListPanes(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if listAsJSON(args) {
		if err := json.NewEncoder(os.Stdout).Encode(panes); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	registry := agents.NewRegistry(cfg.Agents, cfg.AgentOrder)
	if len(panes) == 0 {
		fmt.Println("No panes.")
		return
	}

	for _, pane := range panes {
		agentType := registry.DetectType(pane.Command)
		icon := ""
		if agentType != "" {
			if agent, ok := registry.Get(agentType); ok {
				icon = agent.Icon + " "
			}
		}
		active := ""
		if pane.Active {
			active = " *"
		}
		window := pane.WindowName
		if window != "" {
			window += " "
		}
		fmt.Printf("  %s  %s%s%s  %s%s\n", pane.ID, window, icon, pane.Command, pane.CWD, active)
	}
}

func listAsJSON(args []string) bool {
	if len(args) > 0 && args[0] == "--json" {
		return true
	}
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice == 0
}

func runLayout(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency layout <tiled|columns|rows|main-vertical>")
		os.Exit(1)
	}
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "layout:"+args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if resp != "ok" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp)
		os.Exit(1)
	}
}

func runBroadcastDialog() {
	agencyBin, _ := os.Executable()
	if agencyBin == "" {
		agencyBin = "agency"
	}
	if err := palette.RunBroadcastDialog(agencyBin); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runBroadcastKeys(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency broadcast-keys <text>")
		os.Exit(1)
	}
	text := strings.Join(args, " ")
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "broadcast-keys:"+text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if resp != "ok" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp)
		os.Exit(1)
	}
}

func runRelayout() {
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "relayout")
	if err != nil {
		// Silently fail — this is called from tmux hooks and agency may not be running.
		os.Exit(0)
	}
	if resp != "ok" {
		os.Exit(1)
	}
}

func runAttach() {
	cfg := loadConfig()
	confDir, err := os.UserConfigDir()
	confPath := ""
	if err == nil {
		confPath = filepath.Join(confDir, "agency", "tmux.conf")
		if _, statErr := os.Stat(confPath); statErr != nil {
			confPath = ""
		}
	}
	tc := tmux.NewClient(cfg.Session.Name, confPath)
	ctx := context.Background()
	if !tc.SessionExists(ctx) {
		fmt.Fprintln(os.Stderr, "No agency session running. Start one with: agency")
		os.Exit(1)
	}
	if err := tc.Attach(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error attaching: %v\n", err)
		os.Exit(1)
	}
}

func runConfig() {
	cfg := loadConfig()
	fmt.Printf("Session: %s\n", cfg.Session.Name)
	fmt.Printf("Layout:  %s\n", cfg.Session.DefaultLayout)
	fmt.Printf("Theme:\n")
	fmt.Printf("  Active border:   %s\n", cfg.Theme.ActiveBorder)
	fmt.Printf("  Inactive border: %s\n", cfg.Theme.InactiveBorder)
	fmt.Printf("  Status BG:       %s\n", cfg.Theme.StatusBG)
	fmt.Printf("  Status FG:       %s\n", cfg.Theme.StatusFG)
	fmt.Printf("Agents:\n")
	for _, name := range cfg.AgentOrder {
		agent := cfg.Agents[name]
		fmt.Printf("  %s %s: %s\n", agent.Icon, name, agent.Command)
	}
}

func runLogs() {
	cfg := loadConfig()
	path := logPath(cfg.Session.Name)
	fmt.Println(path)
}

// runDebugMouse instruments the live tmux session's click bindings with
// timestamped logging so dropped mouse-down events (e.g. ghostty#11342) can
// be counted. "agency debug-mouse off" restores the normal bindings.
func runDebugMouse(args []string) {
	cfg := loadConfig()
	ctx := context.Background()
	tc := tmux.NewClient(cfg.Session.Name, "")
	logFile := fmt.Sprintf("/tmp/agency-mouse-%s.log", cfg.Session.Name)

	if len(args) > 0 && args[0] == "off" {
		// Restore the default down binding and agency's up fallback.
		if _, err := tc.Cmd.Run(ctx, "bind", "-T", "root", "MouseDown1Pane", "select-pane -t = ; send-keys -M"); err != nil {
			log.Fatalf("restoring MouseDown1Pane: %v", err)
		}
		if _, err := tc.Cmd.Run(ctx, "bind", "-T", "root", "MouseUp1Pane", tmux.MouseUpPaneFallback); err != nil {
			log.Fatalf("restoring MouseUp1Pane: %v", err)
		}
		fmt.Println("Mouse debug logging off.")
		return
	}

	logPart := func(event string) string {
		return fmt.Sprintf(`run-shell -b 'echo "$(date +%%H:%%M:%%S.%%3N) %s pane=#{mouse_pane} x=#{mouse_x} y=#{mouse_y} active=#{pane_id}" >> %s'`, event, logFile)
	}
	down := logPart("down1") + " ; select-pane -t = ; send-keys -M"
	up := logPart("up1") + " ; " + tmux.MouseUpPaneFallback
	if _, err := tc.Cmd.Run(ctx, "bind", "-T", "root", "MouseDown1Pane", down); err != nil {
		log.Fatalf("binding MouseDown1Pane: %v", err)
	}
	if _, err := tc.Cmd.Run(ctx, "bind", "-T", "root", "MouseUp1Pane", up); err != nil {
		log.Fatalf("binding MouseUp1Pane: %v", err)
	}
	fmt.Printf("Mouse debug logging on. Click around, then:\n  tail -f %s\nEach click should log one down1 and one up1. Missing down1 lines = the terminal dropped the press.\nDisable with: agency debug-mouse off\n", logFile)
}

func runPalette() {
	cfg := loadConfig()
	registry := agents.NewRegistry(cfg.Agents, cfg.AgentOrder)
	bin := agencyBinPath()
	if err := palette.Run(registry, bin); err != nil {
		fmt.Fprintf(os.Stderr, "Palette error: %v\n", err)
		os.Exit(1)
	}
}
