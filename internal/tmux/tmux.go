package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Commander abstracts command execution for testability.
type Commander interface {
	Run(ctx context.Context, args ...string) (string, error)
	Exec(ctx context.Context, args ...string) error
}

// ExecCommander shells out to the real tmux binary.
type ExecCommander struct {
	TmuxBin string // defaults to "tmux"
}

func (e *ExecCommander) bin() string {
	if e.TmuxBin != "" {
		return e.TmuxBin
	}
	return "tmux"
}

func (e *ExecCommander) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

func (e *ExecCommander) Exec(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, e.bin(), args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// PaneInfo represents a tmux pane.
type PaneInfo struct {
	ID      string // e.g. "%0"
	Index   int    // pane index within window
	Command string // running command
	CWD     string // current working directory
	Active  bool   // whether this pane is focused
	PID     int    // pane process PID
}

// Client wraps all tmux CLI interactions.
type Client struct {
	Cmd         Commander
	ConfigPath  string // path to agency's tmux.conf
	SessionName string
}

func NewClient(sessionName, configPath string) *Client {
	return &Client{
		Cmd:         &ExecCommander{},
		SessionName: sessionName,
		ConfigPath:  configPath,
	}
}

func (c *Client) tmuxArgs(args ...string) []string {
	if c.ConfigPath != "" {
		return append([]string{"-f", c.ConfigPath}, args...)
	}
	return args
}

func (c *Client) SessionExists(ctx context.Context) bool {
	_, err := c.Cmd.Run(ctx, "has-session", "-t", c.SessionName)
	return err == nil
}

func (c *Client) NewSession(ctx context.Context) error {
	args := c.tmuxArgs("new-session", "-d", "-s", c.SessionName, "-x", "200", "-y", "50")
	_, err := c.Cmd.Run(ctx, args...)
	return err
}

func (c *Client) KillSession(ctx context.Context) error {
	_, err := c.Cmd.Run(ctx, "kill-session", "-t", c.SessionName)
	return err
}

func (c *Client) Attach(ctx context.Context) error {
	return c.Cmd.Exec(ctx, c.tmuxArgs("attach-session", "-t", c.SessionName)...)
}

// SplitWindow creates a new pane by splitting, running the given command.
// If dir is non-empty, the pane starts in that directory.
func (c *Client) SplitWindow(ctx context.Context, command, dir string) (string, error) {
	args := []string{"split-window", "-t", c.SessionName, "-P", "-F", "#{pane_id}"}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	args = append(args, command)
	out, err := c.Cmd.Run(ctx, args...)
	return strings.TrimSpace(out), err
}

// SetPaneOption sets a per-pane user option (e.g. @agent_color).
func (c *Client) SetPaneOption(ctx context.Context, paneID, option, value string) error {
	_, err := c.Cmd.Run(ctx, "set-option", "-p", "-t", paneID, option, value)
	return err
}

// SetPaneTitle sets the title of a specific pane.
func (c *Client) SetPaneTitle(ctx context.Context, paneID, title string) error {
	_, err := c.Cmd.Run(ctx, "select-pane", "-t", paneID, "-T", title)
	return err
}

// SendKeys sends keystrokes to a pane (used for the initial pane).
func (c *Client) SendKeys(ctx context.Context, paneID, keys string) error {
	_, err := c.Cmd.Run(ctx, "send-keys", "-t", paneID, keys, "Enter")
	return err
}

func (c *Client) KillPane(ctx context.Context, paneID string) error {
	_, err := c.Cmd.Run(ctx, "kill-pane", "-t", paneID)
	return err
}

func (c *Client) ListPanes(ctx context.Context) ([]PaneInfo, error) {
	format := "#{pane_id}\t#{pane_index}\t#{pane_current_command}\t#{pane_current_path}\t#{pane_active}\t#{pane_pid}"
	out, err := c.Cmd.Run(ctx,
		"list-panes", "-t", c.SessionName, "-F", format,
	)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}

	var panes []PaneInfo
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 6)
		if len(parts) < 6 {
			continue
		}
		idx, _ := strconv.Atoi(parts[1])
		pid, _ := strconv.Atoi(parts[5])
		panes = append(panes, PaneInfo{
			ID:      parts[0],
			Index:   idx,
			Command: parts[2],
			CWD:     parts[3],
			Active:  parts[4] == "1",
			PID:     pid,
		})
	}
	return panes, nil
}

func (c *Client) SelectLayout(ctx context.Context, layout string) error {
	_, err := c.Cmd.Run(ctx, "select-layout", "-t", c.SessionName, layout)
	return err
}

// CapturePaneContent captures the last N lines of a pane's visible content.
func (c *Client) CapturePaneContent(ctx context.Context, paneID string, lines int) (string, error) {
	start := fmt.Sprintf("-%d", lines)
	return c.Cmd.Run(ctx,
		"capture-pane", "-t", paneID, "-p", "-S", start,
	)
}

// SetPaneBorderFormat sets the pane border format for all panes in the session.
func (c *Client) SetPaneBorderFormat(ctx context.Context, format string) error {
	_, err := c.Cmd.Run(ctx,
		"set-option", "-t", c.SessionName, "pane-border-format", format,
	)
	return err
}

// SetEnv sets an environment variable in the tmux session.
func (c *Client) SetEnv(ctx context.Context, key, value string) error {
	_, err := c.Cmd.Run(ctx,
		"set-environment", "-t", c.SessionName, key, value,
	)
	return err
}

// DisplayPopup opens a tmux popup running the given command.
func (c *Client) DisplayPopup(ctx context.Context, width, height int, command string) error {
	_, err := c.Cmd.Run(ctx,
		"display-popup", "-t", c.SessionName,
		"-w", strconv.Itoa(width), "-h", strconv.Itoa(height),
		"-E", command,
	)
	return err
}

// RespawnPane respawns a dead pane with a new command.
func (c *Client) RespawnPane(ctx context.Context, paneID, command string) error {
	_, err := c.Cmd.Run(ctx,
		"respawn-pane", "-t", paneID, "-k", command,
	)
	return err
}
