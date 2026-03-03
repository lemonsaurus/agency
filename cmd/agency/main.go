package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lemonsaurus/agency/internal/agents"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/ipc"
	"github.com/lemonsaurus/agency/internal/palette"
	"github.com/lemonsaurus/agency/internal/session"
	"github.com/lemonsaurus/agency/internal/status"
	"github.com/lemonsaurus/agency/internal/tmux"
)

func main() {
	if len(os.Args) < 2 {
		runLaunch()
		return
	}

	switch os.Args[1] {
	case "spawn":
		runSpawn(os.Args[2:])
	case "spawn-dialog":
		runSpawnDialog(os.Args[2:])
	case "kill":
		runKill(os.Args[2:])
	case "kill-all":
		runKillAll()
	case "list":
		runList()
	case "layout":
		runLayout(os.Args[2:])
	case "attach":
		runAttach()
	case "config":
		runConfig()
	case "palette":
		runPalette()
	case "logs":
		runLogs()
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
  agency spawn <agent> [dir...]     Spawn agent pane(s) — one per dir (claude, codex, ...)
  agency spawn --cmd "..." [dir]    Spawn arbitrary command
  agency spawn-dialog <agent> [dir] Open directory picker popup, then spawn
  agency kill <pane-id>             Kill a specific pane
  agency kill-all                   Kill all agent panes
  agency list                       List all panes with status
  agency layout <layout>            Switch layout (tiled, columns, rows, main-vertical)
  agency attach                     Reattach to existing session
  agency config                     Print resolved config
  agency palette                    Open command palette (used by tmux keybinding)
  agency logs                       Print path to log file (tail -f it)
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

func runLaunch() {
	cfg := loadConfig()
	sessionName := cfg.Session.Name

	// Set up file logging before anything else.
	setupLogging(sessionName)

	// Acquire lock.
	lockFile, err := acquireLock(lockPath(sessionName))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() {
		lockFile.Close()
		os.Remove(lockPath(sessionName))
	}()

	// Generate tmux config.
	bin := agencyBinPath()
	confPath, err := tmux.GenerateConfig(cfg, bin)
	if err != nil {
		log.Fatalf("Generating tmux config: %v", err)
	}

	// Create tmux client.
	tc := tmux.NewClient(sessionName, confPath)

	// Set up signal handling with double Ctrl+C.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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

	// Create status poller.
	poller := status.NewPoller(tc, func(paneID, agentType, s string) {
		log.Printf("status: %s (%s) → %s", paneID, agentType, s)
	})

	// Create session manager.
	mgr := session.NewManager(tc, registry, cfg, poller)

	// Check if tmux session already exists (crash recovery).
	if tc.SessionExists(ctx) {
		log.Printf("Existing tmux session found, adopting orphan panes...")
	} else {
		if err := tc.NewSession(ctx); err != nil {
			log.Fatalf("Creating tmux session: %v", err)
		}
	}
	// Label all existing panes (initial shell on fresh start, or orphans on recovery).
	if err := mgr.AdoptOrphans(ctx); err != nil {
		log.Printf("Warning: adopting orphans: %v", err)
	}

	// Start IPC socket server.
	sockPath := socketPath(sessionName)
	srv := ipc.NewServer(sockPath, mgr)
	if err := srv.Start(); err != nil {
		log.Fatalf("Starting socket server: %v", err)
	}
	defer srv.Close()

	// Set AGENCY_SOCKET in the tmux session environment.
	if err := tc.SetEnv(ctx, "AGENCY_SOCKET", sockPath); err != nil {
		log.Printf("Warning: setting AGENCY_SOCKET: %v", err)
	}

	// Start status poller.
	go poller.Run(ctx)

	// Attach to tmux (this blocks until detach or session end).
	log.Printf("Attaching to tmux session %q...", sessionName)
	if err := tc.Attach(ctx); err != nil {
		// Attach failing is normal on detach.
		if ctx.Err() == nil {
			log.Printf("Detached from tmux session.")
		}
	}

	cancel()
	log.Println("Shutting down.")
}

func runSpawn(args []string) {
	cfg := loadConfig()
	sockPath := socketPath(cfg.Session.Name)

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency spawn <agent> [dir] or agency spawn --cmd \"command\" [dir]")
		os.Exit(1)
	}

	var msgs []string
	if args[0] == "--cmd" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: agency spawn --cmd \"command\" [dir]")
			os.Exit(1)
		}
		command, dir := extractDirArg(args[1:])
		msgs = []string{"spawn:cmd:" + strings.Join(command, " ") + dirSuffix(dir)}
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
			msgs = append(msgs, "spawn:"+name+dirSuffix(abs))
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

func runSpawnDialog(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency spawn-dialog <agent> [default-dir]")
		os.Exit(1)
	}
	agentName := args[0]
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

func runKill(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency kill <pane-id>")
		os.Exit(1)
	}
	cfg := loadConfig()
	resp, err := ipc.SendMessage(socketPath(cfg.Session.Name), "kill:"+args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if resp != "ok" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp)
		os.Exit(1)
	}
}

func runKillAll() {
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

func runList() {
	cfg := loadConfig()
	tc := tmux.NewClient(cfg.Session.Name, "")
	ctx := context.Background()
	panes, err := tc.ListPanes(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
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
		fmt.Printf("  %s  %s%s  %s%s\n", pane.ID, icon, pane.Command, pane.CWD, active)
	}
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

func runPalette() {
	cfg := loadConfig()
	registry := agents.NewRegistry(cfg.Agents, cfg.AgentOrder)
	bin := agencyBinPath()
	if err := palette.Run(registry, bin); err != nil {
		fmt.Fprintf(os.Stderr, "Palette error: %v\n", err)
		os.Exit(1)
	}
}
