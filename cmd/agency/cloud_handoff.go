package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// handoffCloud moves a Pi session to the cloud host: the branch is committed
// and pushed, the box checks it out in the matching directory, the session
// file is copied into the box's session store, and a remote Pi resumes it.
type handoffCloud struct {
	remote  *cloud.Client
	dir     string
	session string
	label   string
	window  string
	prompt  string
}

func runHandoffCloud(args []string) {
	cfg := loadConfig()
	h := handoffCloud{remote: &cloud.Client{Host: cfg.Cloud.Host}}
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Error: handoff-cloud option needs a value")
			os.Exit(1)
		}
		switch args[0] {
		case "--session":
			h.session = args[1]
		case "--label":
			h.label = args[1]
		case "--prompt":
			h.prompt = args[1]
		default:
			fmt.Fprintf(os.Stderr, "Error: unknown handoff-cloud option %s\n", args[0])
			os.Exit(1)
		}
		args = args[2:]
	}
	if len(args) != 1 || h.session == "" {
		fmt.Fprintln(os.Stderr, "Usage: agency handoff-cloud [--label task] [--prompt text] --session <file> <dir>")
		os.Exit(1)
	}
	if cfg.Cloud.Host == "" {
		fmt.Fprintln(os.Stderr, "Error: [cloud] host is not configured")
		os.Exit(1)
	}
	dir, ok := resolveDir(args[0])
	if !ok {
		os.Exit(1)
	}
	h.dir = dir
	// The sky window matches the local one.
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		name, err := tmux.NewClient(cfg.Session.Name, "").Cmd.Run(context.Background(), "display-message", "-p", "-t", pane, "#{window_name}")
		if err == nil {
			h.window = name
		}
	}
	// Handoffs from one window run together and would race on shared checkouts.
	lock, err := os.OpenFile(lockPath(cfg.Session.Name+"-handoff-cloud"), os.O_CREATE|os.O_RDWR, 0o600)
	if err == nil {
		defer lock.Close()
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
	}
	if err == nil {
		err = h.run(context.Background())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// checkout is one working tree that travels with the session.
type checkout struct {
	dir    string
	branch string
}

func (h *handoffCloud) run(ctx context.Context) error {
	dirs, err := h.checkoutsToPublish()
	if err != nil {
		return err
	}
	var checkouts []checkout
	for _, dir := range dirs {
		branch, err := publishBranch(dir)
		if err != nil {
			return err
		}
		checkouts = append(checkouts, checkout{dir: dir, branch: branch})
	}
	remoteHome, err := h.remote.Shell(ctx, 20*time.Second, "echo \"$HOME\"")
	if err != nil {
		return err
	}
	remoteHome = strings.TrimSpace(remoteHome)
	mainRoot, err := gitIn(h.dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	remoteMain, err := swapHome(filepath.Dir(mainRoot), remoteHome)
	if err != nil {
		return err
	}
	var script strings.Builder
	var remoteDir string
	for _, c := range checkouts {
		dir, err := swapHome(c.dir, remoteHome)
		if err != nil {
			return err
		}
		if c.dir == h.dir {
			remoteDir = dir
		}
		fmt.Printf("checking out %s in %s:%s\n", c.branch, h.remote.Host, dir)
		linked := ""
		if dir != remoteMain {
			linked = remoteMain
		}
		script.WriteString(remoteCheckout(linked, dir, c.branch))
	}
	if _, err := h.remote.Shell(ctx, 5*time.Minute, script.String()); err != nil {
		return err
	}
	branch := checkouts[0].branch
	// Pi writes the session file with the first message; an empty session
	// becomes a fresh Pi.
	command := `PATH="$HOME/.local/bin:$PATH" pi`
	if _, err := os.Stat(h.session); err == nil {
		remoteSession, err := h.copySession(ctx, remoteHome, remoteDir)
		if err != nil {
			return err
		}
		prompt := h.prompt
		if prompt == "" {
			prompt = fmt.Sprintf("This session moved from Lemon's workstation to the cloud harness box. Same repo on branch %s, with the local work committed and pushed. Tell Lemon you arrived, then continue the active work.", branch)
		}
		command += fmt.Sprintf(" --session %s %s", shellQuote(remoteSession), shellQuote(prompt))
	}
	spawn := []string{"spawn"}
	if h.window != "" {
		spawn = append(spawn, "--window", h.window)
	}
	if h.label != "" {
		spawn = append(spawn, "--label", h.label)
	}
	spawn = append(spawn, "--cmd", command, remoteDir)
	if _, err := h.remote.Run(ctx, 30*time.Second, spawn...); err != nil {
		return err
	}
	runSyncCloud(true)
	fmt.Printf("handed off to %s:%s on %s\n", h.remote.Host, remoteDir, branch)
	return nil
}

// checkoutsToPublish is the session's directory followed by every other
// worktree of the same repository holding dirty or unpushed work.
func (h *handoffCloud) checkoutsToPublish() ([]string, error) {
	top, err := gitIn(h.dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not a git checkout", h.dir)
	}
	dirs := []string{h.dir}
	list, err := gitIn(h.dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(list, "\n") {
		dir, ok := strings.CutPrefix(line, "worktree ")
		if !ok || dir == top {
			continue
		}
		if unpublished(dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

func unpublished(dir string) bool {
	status, err := gitIn(dir, "status", "--porcelain")
	if err != nil || status != "" {
		return err == nil
	}
	ahead, err := gitIn(dir, "rev-list", "--count", "HEAD", "--not", "--remotes")
	return err == nil && ahead != "0"
}

// publishBranch commits dirty work and pushes the branch. Work on main or a
// detached HEAD moves to a fresh handoff branch first. The name satisfies
// Journalia's ruleset: <owner>/micro-fix/<slug>. A clean, pushed main stays
// main.
func publishBranch(dir string) (string, error) {
	branch, err := gitIn(dir, "branch", "--show-current")
	if err != nil {
		return "", fmt.Errorf("%s is not a git checkout", dir)
	}
	if (branch == "main" || branch == "master") && !unpublished(dir) {
		return branch, nil
	}
	if branch == "" || branch == "main" || branch == "master" {
		branch = fmt.Sprintf("%s/micro-fix/handoff-%s", branchOwner(), time.Now().Format("20060102-1504"))
		if _, err := gitIn(dir, "checkout", "-b", branch); err != nil {
			return "", err
		}
	}
	status, err := gitIn(dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		if _, err := gitIn(dir, "add", "-A"); err != nil {
			return "", err
		}
		if _, err := gitIn(dir, "commit", "-q", "--no-verify", "-m", "wip: cloud handoff"); err != nil {
			return "", err
		}
	}
	fmt.Printf("pushing %s from %s\n", branch, dir)
	if _, err := gitIn(dir, "push", "-q", "--no-verify", "-u", "origin", branch); err != nil {
		return "", err
	}
	return branch, nil
}

func (h *handoffCloud) copySession(ctx context.Context, remoteHome, remoteDir string) (string, error) {
	sessionDir := filepath.Join(remoteHome, ".pi", "agent", "sessions", sessionSlug(remoteDir))
	if _, err := h.remote.Shell(ctx, 20*time.Second, "mkdir -p "+shellQuote(sessionDir)); err != nil {
		return "", err
	}
	target := filepath.Join(sessionDir, filepath.Base(h.session))
	local, err := rewriteSessionCwd(h.session, remoteDir)
	if err != nil {
		return "", err
	}
	defer os.Remove(local)
	fmt.Printf("copying session to %s\n", target)
	return target, h.remote.Copy(ctx, 2*time.Minute, local, target)
}

// rewriteSessionCwd writes a copy of the session whose header points at the
// remote directory, so Pi resumes without asking about a missing cwd.
func rewriteSessionCwd(session, cwd string) (string, error) {
	data, err := os.ReadFile(session)
	if err != nil {
		return "", err
	}
	header, rest, _ := bytes.Cut(data, []byte("\n"))
	var fields map[string]any
	if err := json.Unmarshal(header, &fields); err != nil || fields["type"] != "session" {
		return "", fmt.Errorf("%s is not a Pi session file", session)
	}
	fields["cwd"] = cwd
	header, err = json.Marshal(fields)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "handoff-*.jsonl")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	for _, chunk := range [][]byte{header, []byte("\n"), rest} {
		if _, err := tmp.Write(chunk); err != nil {
			return "", err
		}
	}
	return tmp.Name(), nil
}

func gitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// remoteCheckout fetches the branch and checks it out. A linked worktree is
// created under the main repository when the directory is missing. A dirty
// remote checkout aborts instead of being overwritten.
func remoteCheckout(remoteMain, remoteDir, branch string) string {
	lines := []string{"set -e"}
	if remoteMain != "" {
		lines = append(lines,
			"cd "+shellQuote(remoteMain),
			"git fetch -q origin "+shellQuote(branch),
			"if [ ! -d "+shellQuote(remoteDir)+" ]; then git worktree add -q "+shellQuote(remoteDir)+" -B "+shellQuote(branch)+" origin/"+shellQuote(branch)+"; fi",
		)
	}
	lines = append(lines,
		"cd "+shellQuote(remoteDir),
		"git fetch -q origin "+shellQuote(branch),
		`if [ -n "$(git status --porcelain)" ]; then echo "remote checkout is dirty: $PWD" >&2; exit 3; fi`,
		"git checkout -q -B "+shellQuote(branch)+" origin/"+shellQuote(branch),
	)
	return strings.Join(lines, "\n") + "\n"
}

// swapHome rewrites a path under the local home directory onto another home.
func swapHome(path, remoteHome string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, candidate := range []string{home, realHome(home)} {
		if rel, ok := strings.CutPrefix(path, candidate+"/"); ok {
			return filepath.Join(remoteHome, rel), nil
		}
		if path == candidate {
			return remoteHome, nil
		}
	}
	if link, ok := homeLink(path, home); ok {
		return swapHome(link, remoteHome)
	}
	return "", fmt.Errorf("%s is outside the home directory", path)
}

// homeLink finds the ~/git/<org>/<repo> symlink that points at path or one of
// its parents, e.g. a checkout on a Windows drive.
func homeLink(path, home string) (string, bool) {
	links, _ := filepath.Glob(filepath.Join(home, "git", "*", "*"))
	for _, link := range links {
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := filepath.EvalSymlinks(link)
		if err != nil {
			continue
		}
		if path == target {
			return link, true
		}
		if rel, ok := strings.CutPrefix(path, target+"/"); ok {
			return filepath.Join(link, rel), true
		}
	}
	return "", false
}

func branchOwner() string {
	for _, name := range []string{"USER", "LOGNAME"} {
		if value := strings.ToLower(os.Getenv(name)); value != "" {
			return value
		}
	}
	return "handoff"
}

func realHome(home string) string {
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		return home
	}
	return resolved
}

// sessionSlug is Pi's per-directory session folder name.
func sessionSlug(dir string) string {
	return "-" + strings.ReplaceAll(dir, "/", "-") + "--"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
