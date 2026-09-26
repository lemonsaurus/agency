// Package cloud talks to a headless `agency serve` on another host over SSH.
package cloud

import (
	"bufio"
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

	"github.com/lemonsaurus/agency/internal/tmux"
)

// DefaultGroup is the remote-world window for agents spawned without one.
const DefaultGroup = "main"

// Placeholder is the viewer id of the pane that keeps an empty or unreachable
// remote world open.
const Placeholder = "none"

// Group is the remote-world window a remote agent belongs in.
func Group(pane tmux.PaneInfo) string {
	if pane.Group == "" {
		return DefaultGroup
	}
	return pane.Group
}

// Client runs `agency cloud ...` on the remote host.
type Client struct {
	Host string
}

// Window is one remote agent: a single-pane window on the headless server.
type Window struct {
	ID   string // tmux window id, e.g. "@3"
	Name string // remote pane label, e.g. "π pi@journalia"
	Pane tmux.PaneInfo
}

// controlPath is the cloud-harness installer's, so viewers, the watch, and
// commands share the master that carries its browser link.
const controlPath = "ControlPath=~/.ssh/cloud-harness-%C"

// sshOptions join the shared master and skip the config's forwards. A
// connection the master refuses runs on its own, and its copy of the link
// forward would rebind link.sock on the box away from the master's listener.
var sshOptions = []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ControlMaster=no", "-o", controlPath, "-o", "ClearAllForwardings=yes"}

// ensureMaster starts the shared master unless one is running. The lock keeps
// concurrent Agency processes from racing to become it.
func (c *Client) ensureMaster(ctx context.Context) error {
	lock, err := os.OpenFile(filepath.Join(os.TempDir(), "agency-ssh-master.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if exec.CommandContext(ctx, "ssh", "-o", controlPath, "-O", "check", c.Host).Run() == nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ControlMaster=yes", "-o", controlPath, "-o", "ControlPersist=10m", "-o", "ServerAliveInterval=15", c.Host, "true")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh master for %s: %w: %s", c.Host, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// sshArgs builds a non-interactive SSH invocation. ~/.local/bin is only on
// PATH in interactive shells on the box, so the remote command adds it.
func (c *Client) sshArgs(tty bool, remote ...string) []string {
	args := append([]string{}, sshOptions...)
	if tty {
		args = append(args, "-t")
	}
	quoted := make([]string, len(remote))
	for i, arg := range remote {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	command := "PATH=\"$HOME/.local/bin:$PATH\" agency cloud " + strings.Join(quoted, " ")
	return append(args, c.Host, command)
}

// Run executes a remote `agency cloud` subcommand and returns its stdout.
func (c *Client) Run(ctx context.Context, timeout time.Duration, remote ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.ensureMaster(ctx); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "ssh", c.sshArgs(false, remote...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh %s agency cloud %s: %w: %s", c.Host, strings.Join(remote, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Shell runs a bash script on the host and returns its stdout.
func (c *Client) Shell(ctx context.Context, timeout time.Duration, script string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.ensureMaster(ctx); err != nil {
		return "", err
	}
	args := append(append([]string{}, sshOptions...), c.Host, "bash -s")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh %s: %w: %s", c.Host, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Copy uploads a local file to a path on the host.
func (c *Client) Copy(ctx context.Context, timeout time.Duration, local, remote string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.ensureMaster(ctx); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "scp", append(append([]string{"-q"}, sshOptions...), local, c.Host+":"+remote)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scp to %s: %w: %s", c.Host, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Watch calls changed for every change notice from the host's `agency cloud
// watch` until the link drops or ctx ends. The open stdin pipe keeps the
// remote side alive.
func (c *Client) Watch(ctx context.Context, changed func()) error {
	if err := c.ensureMaster(ctx); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ssh", c.sshArgs(false, "watch")...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		changed()
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Attach hands this terminal to one remote window until the link drops or
// the window dies.
func (c *Client) Attach(ctx context.Context, windowID string) error {
	if err := c.ensureMaster(ctx); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ssh", c.sshArgs(true, "attach", windowID)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Windows lists remote agents, one per window.
func (c *Client) Windows(ctx context.Context) ([]Window, error) {
	out, err := c.Run(ctx, 15*time.Second, "list", "--json")
	if err != nil {
		return nil, err
	}
	var panes []tmux.PaneInfo
	if err := json.Unmarshal([]byte(out), &panes); err != nil {
		return nil, fmt.Errorf("parsing remote pane list: %w", err)
	}
	var windows []Window
	seen := map[string]bool{}
	for _, pane := range panes {
		if pane.WindowID == "" || seen[pane.WindowID] {
			continue
		}
		seen[pane.WindowID] = true
		windows = append(windows, Window{ID: pane.WindowID, Name: pane.WindowName, Pane: pane})
	}
	return windows, nil
}
