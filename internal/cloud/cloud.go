// Package cloud talks to a headless `agency serve` on another host over SSH.
package cloud

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lemonsaurus/agency/internal/retry"
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

// ErrUnreachable marks an SSH connection that failed or stalled, as opposed to
// a remote command that ran and failed.
var ErrUnreachable = errors.New("sky unreachable")

// Client runs `agency cloud ...` on the remote host.
type Client struct {
	Host string
	// Retrying, when set, makes Run, Shell and Copy retry an unreachable host
	// and hears about each retry.
	Retrying func(attempt, attempts int, err error)
}

// unreachable keeps the SSH error text and matches ErrUnreachable.
type unreachable struct {
	error
	stalled bool
}

func (unreachable) Is(target error) bool { return target == ErrUnreachable }

func (u unreachable) Unwrap() error { return u.error }

// sshFailure marks err unreachable when ssh itself failed: ssh exits 255 for
// its own errors and with the remote command's status otherwise.
func sshFailure(err, cause error) error {
	var exit *exec.ExitError
	if errors.As(cause, &exit) && exit.ExitCode() == 255 {
		return unreachable{error: err}
	}
	return err
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
		return sshFailure(fmt.Errorf("ssh master for %s: %w: %s", c.Host, err, strings.TrimSpace(stderr.String())), err)
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

// do runs one SSH operation under timeout, retrying an unreachable host when
// Retrying is set.
func (c *Client) do(ctx context.Context, timeout time.Duration, op func(context.Context) error) error {
	if c.Retrying == nil {
		return c.attempt(ctx, timeout, op)
	}
	return retry.Do(ctx, func(err error) bool { return errors.Is(err, ErrUnreachable) }, c.Retrying, func() error {
		err := c.attempt(ctx, timeout, op)
		if u, ok := err.(unreachable); ok && u.stalled {
			c.dropStaleMaster(ctx)
		}
		return err
	})
}

// attempt marks an operation that outlived its own timeout as stalled.
func (c *Client) attempt(ctx context.Context, timeout time.Duration, op func(context.Context) error) error {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := c.ensureMaster(attemptCtx)
	if err == nil {
		err = op(attemptCtx)
	}
	if err != nil && ctx.Err() == nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
		return unreachable{error: err, stalled: true}
	}
	return err
}

// dropStaleMaster closes a master that no longer answers. After the network
// changes, the master process lives on until ServerAliveInterval notices, and
// every session through it hangs; closing it lets the next attempt start fresh.
func (c *Client) dropStaleMaster(ctx context.Context) {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if exec.CommandContext(probe, "ssh", append(append([]string{}, sshOptions...), c.Host, "true")...).Run() == nil {
		return
	}
	_ = exec.CommandContext(ctx, "ssh", "-o", controlPath, "-O", "exit", c.Host).Run()
}

// Run executes a remote `agency cloud` subcommand and returns its stdout.
func (c *Client) Run(ctx context.Context, timeout time.Duration, remote ...string) (string, error) {
	var out string
	err := c.do(ctx, timeout, func(ctx context.Context) error {
		cmd := exec.CommandContext(ctx, "ssh", c.sshArgs(false, remote...)...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return sshFailure(fmt.Errorf("ssh %s agency cloud %s: %w: %s", c.Host, strings.Join(remote, " "), err, strings.TrimSpace(stderr.String())), err)
		}
		out = stdout.String()
		return nil
	})
	return out, err
}

// Shell runs a bash script on the host and returns its stdout.
func (c *Client) Shell(ctx context.Context, timeout time.Duration, script string) (string, error) {
	var out string
	err := c.do(ctx, timeout, func(ctx context.Context) error {
		args := append(append([]string{}, sshOptions...), c.Host, "bash -s")
		cmd := exec.CommandContext(ctx, "ssh", args...)
		cmd.Stdin = strings.NewReader(script)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return sshFailure(fmt.Errorf("ssh %s: %w: %s", c.Host, err, strings.TrimSpace(stderr.String())), err)
		}
		out = stdout.String()
		return nil
	})
	return out, err
}

// Copy uploads a local file to a path on the host. It streams through ssh
// rather than scp, whose exit status cannot tell a dropped connection from a
// remote write error.
func (c *Client) Copy(ctx context.Context, timeout time.Duration, local, remote string) error {
	return c.do(ctx, timeout, func(ctx context.Context) error {
		file, err := os.Open(local)
		if err != nil {
			return err
		}
		defer file.Close()
		args := append(append([]string{}, sshOptions...), c.Host, "cat > '"+strings.ReplaceAll(remote, "'", `'\''`)+"'")
		cmd := exec.CommandContext(ctx, "ssh", args...)
		cmd.Stdin = file
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return sshFailure(fmt.Errorf("copy to %s: %w: %s", c.Host, err, strings.TrimSpace(stderr.String())), err)
		}
		return nil
	})
}

// WatchHandlers receive what the host's `agency cloud watch` prints.
type WatchHandlers struct {
	Up       func()               // the host printed its first line: the link is up
	Changed  func()               // the panes the sky harness mirrors changed
	Reminder func(payload string) // a "reminder {json}" line
	Views    func(payload string) // a "views ..." line: what the host did with a shown set
	Lease    func(payload string) // a "lease {json}" line: who holds the sky harness now
}

// Watch reports the host's lines to handlers until the link drops or ctx
// ends, and writes every line from shown and requests to the host. The open
// stdin pipe keeps the remote side alive.
func (c *Client) Watch(ctx context.Context, handlers WatchHandlers, shown, requests <-chan string) error {
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
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case line := <-shown:
				if _, err := fmt.Fprintln(stdin, line); err != nil {
					return
				}
			case line := <-requests:
				if _, err := fmt.Fprintln(stdin, line); err != nil {
					return
				}
			}
		}
	}()
	lines := bufio.NewScanner(stdout)
	for up := false; lines.Scan(); {
		if !up {
			up = true
			handlers.Up()
		}
		if payload, ok := strings.CutPrefix(lines.Text(), "lease "); ok {
			handlers.Lease(payload)
			continue
		}
		if payload, ok := strings.CutPrefix(lines.Text(), "reminder "); ok {
			handlers.Reminder(payload)
			continue
		}
		if payload, ok := strings.CutPrefix(lines.Text(), "views "); ok {
			handlers.Views(payload)
			continue
		}
		handlers.Changed()
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Attach hands this terminal to one remote window until the link drops or
// the window dies. A viewer key lets the host park the attachment while the
// device does not show it.
func (c *Client) Attach(ctx context.Context, windowID, viewer string) error {
	if err := c.ensureMaster(ctx); err != nil {
		return err
	}
	args := []string{"attach", windowID}
	if viewer != "" {
		args = append(args, "--viewer", viewer)
	}
	cmd := exec.CommandContext(ctx, "ssh", c.sshArgs(true, args...)...)
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
