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
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
)

// handoffCloud moves a Pi session to the cloud host: the branch is committed
// and pushed, the box checks it out in the matching directory, the session
// file is copied into the box's session store, and a remote Pi resumes it.
type handoffCloud struct {
	remote  *cloud.Client
	dir     string
	session string
	label   string
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
	if err := h.run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func (h *handoffCloud) run(ctx context.Context) error {
	branch, err := h.publishBranch()
	if err != nil {
		return err
	}
	remoteHome, err := h.remote.Shell(ctx, 20*time.Second, "echo \"$HOME\"")
	if err != nil {
		return err
	}
	remoteHome = strings.TrimSpace(remoteHome)
	remoteDir, remoteMain, err := h.remotePaths(remoteHome)
	if err != nil {
		return err
	}
	fmt.Printf("checking out %s in %s:%s\n", branch, h.remote.Host, remoteDir)
	if _, err := h.remote.Shell(ctx, 3*time.Minute, remoteCheckout(remoteMain, remoteDir, branch)); err != nil {
		return err
	}
	remoteSession, err := h.copySession(ctx, remoteHome, remoteDir)
	if err != nil {
		return err
	}
	prompt := h.prompt
	if prompt == "" {
		prompt = fmt.Sprintf("This session moved from Lemon's workstation to the cloud harness box. Same repo on branch %s, with the local work committed and pushed. Tell Lemon you arrived, then continue the active work.", branch)
	}
	command := fmt.Sprintf(`PATH="$HOME/.local/bin:$PATH" pi --session %s %s`, shellQuote(remoteSession), shellQuote(prompt))
	spawn := []string{"spawn"}
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

// publishBranch commits dirty work and pushes the branch. Work on main or a
// detached HEAD moves to a fresh handoff branch first. The name satisfies
// Journalia's ruleset: <owner>/micro-fix/<slug>.
func (h *handoffCloud) publishBranch() (string, error) {
	branch, err := h.git("branch", "--show-current")
	if err != nil {
		return "", fmt.Errorf("%s is not a git checkout", h.dir)
	}
	if branch == "" || branch == "main" || branch == "master" {
		branch = fmt.Sprintf("%s/micro-fix/handoff-%s", branchOwner(), time.Now().Format("20060102-1504"))
		if _, err := h.git("checkout", "-b", branch); err != nil {
			return "", err
		}
	}
	status, err := h.git("status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		if _, err := h.git("add", "-A"); err != nil {
			return "", err
		}
		if _, err := h.git("commit", "-q", "--no-verify", "-m", "wip: cloud handoff"); err != nil {
			return "", err
		}
	}
	fmt.Printf("pushing %s\n", branch)
	if _, err := h.git("push", "-q", "--no-verify", "-u", "origin", branch); err != nil {
		return "", err
	}
	return branch, nil
}

// remotePaths maps the checkout and its main repository root onto the box by
// swapping home directories.
func (h *handoffCloud) remotePaths(remoteHome string) (string, string, error) {
	top, err := h.git("rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", err
	}
	common, err := h.git("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", "", err
	}
	mainRoot := filepath.Dir(common)
	remoteDir, err := swapHome(h.dir, remoteHome)
	if err != nil {
		return "", "", err
	}
	remoteMain, err := swapHome(mainRoot, remoteHome)
	if err != nil {
		return "", "", err
	}
	if top == mainRoot {
		return remoteDir, "", nil
	}
	return remoteDir, remoteMain, nil
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

func (h *handoffCloud) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", h.dir}, args...)...)
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
	return "", fmt.Errorf("%s is outside the home directory", path)
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
