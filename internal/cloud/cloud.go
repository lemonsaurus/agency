// Package cloud talks to a headless `agency serve` on another host over SSH.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lemonsaurus/agency/internal/tmux"
)

// WindowName is the local window that mirrors the remote panes.
const WindowName = "cloud-harness"

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

// sshArgs builds a non-interactive SSH invocation. ~/.local/bin is only on
// PATH in interactive shells on the box, so the remote command adds it.
func (c *Client) sshArgs(tty bool, remote ...string) []string {
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=15"}
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
	cmd := exec.CommandContext(ctx, "ssh", c.sshArgs(false, remote...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh %s agency cloud %s: %w: %s", c.Host, strings.Join(remote, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Attach hands this terminal to one remote window until the link drops or
// the window dies.
func (c *Client) Attach(ctx context.Context, windowID string) error {
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
