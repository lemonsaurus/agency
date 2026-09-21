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
	TmuxBin    string // defaults to "tmux"
	SocketName string // tmux -L server name; empty for the default server
}

func (e *ExecCommander) bin() string {
	if e.TmuxBin != "" {
		return e.TmuxBin
	}
	return "tmux"
}

func (e *ExecCommander) args(args []string) []string {
	if e.SocketName == "" {
		return args
	}
	return append([]string{"-L", e.SocketName}, args...)
}

func (e *ExecCommander) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.bin(), e.args(args)...)
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
	cmd := exec.CommandContext(ctx, e.bin(), e.args(args)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// PaneInfo represents a tmux pane.
type PaneInfo struct {
	ID               string `json:"id"`                 // e.g. "%0"
	Index            int    `json:"index"`              // pane index within window
	WindowIndex      int    `json:"windowIndex"`        // tmux window index
	WindowID         string `json:"windowId,omitempty"` // e.g. "@3", stable for the server's life
	WindowName       string `json:"windowName"`         // tmux window name
	Command          string `json:"command"`            // running command
	CWD              string `json:"cwd"`                // current working directory
	Active           bool   `json:"active"`             // whether this pane is focused
	PID              int    `json:"pid"`                // pane process PID
	Role             string `json:"role,omitempty"`
	ParentID         string `json:"parentId,omitempty"`
	RootID           string `json:"rootId,omitempty"`
	PendingPromotion string `json:"pendingPromotion,omitempty"`
	AgencyCommand    string `json:"agencyCommand,omitempty"`
	CloudWindow      string `json:"cloudWindow,omitempty"` // remote window id this local pane views
}

type windowRef struct {
	ID    string
	Index int
	Name  string
}

// Client wraps all tmux CLI interactions.
type Client struct {
	Cmd         Commander
	ConfigPath  string // path to agency's tmux.conf
	SessionName string
}

func NewClient(sessionName, configPath string) *Client {
	return &Client{
		Cmd:         &ExecCommander{SocketName: os.Getenv("AGENCY_TMUX_SOCKET")},
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
	args := c.tmuxArgs("new-session", "-d", "-s", c.SessionName, "-n", "control", "-x", "200", "-y", "50")
	_, err := c.Cmd.Run(ctx, args...)
	return err
}

func (c *Client) SourceConfig(ctx context.Context) error {
	if c.ConfigPath == "" {
		return nil
	}
	_, err := c.Cmd.Run(ctx, "source-file", c.ConfigPath)
	return err
}

func (c *Client) KillSession(ctx context.Context) error {
	_, err := c.Cmd.Run(ctx, "kill-session", "-t", c.SessionName)
	return err
}

func (c *Client) Attach(ctx context.Context) error {
	return c.Cmd.Exec(ctx, c.tmuxArgs("attach-session", "-t", c.SessionName)...)
}

// AttachWindow attaches this client to a single window through a private
// session grouped with the main one, so each viewer keeps its own current
// window. The view session is removed once the client detaches; the window
// and its agent stay with the main session.
func (c *Client) AttachWindow(ctx context.Context, windowID string) error {
	view := fmt.Sprintf("view-%d", os.Getpid())
	if _, err := c.Cmd.Run(ctx, "new-session", "-d", "-t", c.SessionName, "-s", view); err != nil {
		return err
	}
	defer c.Cmd.Run(ctx, "kill-session", "-t", view)
	if _, err := c.Cmd.Run(ctx, "select-window", "-t", view+":"+windowID); err != nil {
		return err
	}
	// destroy-unattached is set from inside the attached client, so a link
	// that dies before the deferred kill still takes the view session with it.
	return c.Cmd.Exec(ctx, "attach-session", "-t", view, ";", "set-option", "-t", view, "destroy-unattached", "on")
}

// SplitWindow creates a new pane by splitting, running the given command.
// If dir is non-empty, the pane starts in that directory.
func (c *Client) SplitWindow(ctx context.Context, command, dir string) (string, error) {
	return c.splitWithRetile(ctx, c.SessionName, command, dir)
}

func (c *Client) SplitWindowInWindow(ctx context.Context, windowName, command, dir string) (string, error) {
	return c.splitWithRetile(ctx, c.windowTarget(windowName), command, dir)
}

// SplitWindowAt creates a pane in the window containing targetPaneID.
func (c *Client) SplitWindowAt(ctx context.Context, targetPaneID, command, dir string) (string, error) {
	return c.splitWithRetile(ctx, targetPaneID, command, dir)
}

func (c *Client) firstWindow(ctx context.Context) (windowRef, error) {
	windows, err := c.listWindowRefs(ctx)
	if err != nil {
		return windowRef{}, err
	}
	if len(windows) == 0 {
		return windowRef{}, fmt.Errorf("session has no windows")
	}
	first := windows[0]
	for _, window := range windows[1:] {
		if window.Index < first.Index {
			first = window
		}
	}
	return first, nil
}

func (c *Client) NameFirstWindow(ctx context.Context, name string) error {
	first, err := c.firstWindow(ctx)
	if err != nil || first.Name == name {
		return err
	}
	target := first.ID
	if target == "" {
		target = c.windowTarget(first.Name)
	}
	_, err = c.Cmd.Run(ctx, "rename-window", "-t", target, name)
	return err
}

// SplitWindowInFirstWindow creates a pane in the session's lowest-indexed window.
func (c *Client) SplitWindowInFirstWindow(ctx context.Context, command, dir string) (string, string, error) {
	first, err := c.firstWindow(ctx)
	if err != nil {
		return "", "", err
	}
	target := first.ID
	if target == "" {
		target = c.windowTarget(first.Name)
	}
	paneID, err := c.splitWithRetile(ctx, target, command, dir)
	return paneID, first.Name, err
}

// splitWithRetile splits the target's active pane. When the active pane is too
// small tmux fails with "no space for a new pane"; retile the window and retry
// once.
func (c *Client) splitWithRetile(ctx context.Context, target, command, dir string) (string, error) {
	args := []string{"split-window", "-t", target, "-P", "-F", "#{pane_id}"}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	args = append(args, command)
	out, err := c.Cmd.Run(ctx, args...)
	if err != nil && strings.Contains(err.Error()+out, "no space for a new pane") {
		if _, layoutErr := c.Cmd.Run(ctx, "select-layout", "-t", target, "tiled"); layoutErr == nil {
			out, err = c.Cmd.Run(ctx, args...)
		}
	}
	return strings.TrimSpace(out), err
}

func (c *Client) NewWindow(ctx context.Context, name, command, dir string) (string, error) {
	// Trailing colon: a bare session name resolves to its current window,
	// making new-window fail with "index in use". The colon picks the next free index.
	args := []string{"new-window", "-t", c.SessionName + ":", "-n", name, "-P", "-F", "#{pane_id}"}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	args = append(args, command)
	out, err := c.Cmd.Run(ctx, args...)
	return strings.TrimSpace(out), err
}

// ServerPID returns the tmux server's process ID.
func (c *Client) ServerPID(ctx context.Context) (int, error) {
	out, err := c.Cmd.Run(ctx, "display-message", "-p", "#{pid}")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func (c *Client) WindowExists(ctx context.Context, name string) (bool, error) {
	windows, err := c.listWindowRefs(ctx)
	if err != nil {
		return false, err
	}
	for _, window := range windows {
		if window.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) listWindowRefs(ctx context.Context) ([]windowRef, error) {
	out, err := c.Cmd.Run(ctx, "list-windows", "-t", c.SessionName, "-F", "#{window_id}\t#{window_index}\t#{window_name}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	windows := []windowRef{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) == 1 {
			windows = append(windows, windowRef{Name: parts[0]})
			continue
		}
		if len(parts) < 3 {
			continue
		}
		idx, _ := strconv.Atoi(parts[1])
		windows = append(windows, windowRef{ID: parts[0], Index: idx, Name: parts[2]})
	}
	return windows, nil
}

func (c *Client) resolveWindowTarget(ctx context.Context, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("window target is required")
	}
	windows, err := c.listWindowRefs(ctx)
	if err != nil {
		return "", err
	}
	for _, window := range windows {
		if window.ID == target || strconv.Itoa(window.Index) == target || window.Name == target {
			if window.ID != "" {
				return window.ID, nil
			}
			return c.windowTarget(window.Name), nil
		}
	}
	return "", fmt.Errorf("window %q not found", target)
}

// SetPaneOption sets a per-pane user option (e.g. @agent_color).
func (c *Client) SetPaneOption(ctx context.Context, paneID, option, value string) error {
	_, err := c.Cmd.Run(ctx, "set-option", "-p", "-t", paneID, option, value)
	return err
}

// SetWindowOption sets a window user option, targeting the window that
// contains the given pane.
func (c *Client) SetWindowOption(ctx context.Context, paneID, option, value string) error {
	_, err := c.Cmd.Run(ctx, "set-option", "-w", "-t", paneID, option, value)
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

// SendText sends literal text to a pane and optionally presses Enter.
func (c *Client) SendText(ctx context.Context, paneID, text string, enter bool) error {
	if enter {
		text += "\r"
	}
	_, err := c.Cmd.Run(ctx, "send-keys", "-l", "-t", paneID, text)
	return err
}

func (c *Client) DisplayMessage(ctx context.Context, paneID, message string) error {
	_, err := c.Cmd.Run(ctx, "display-message", "-t", paneID, "-d", "6000", "--", message)
	return err
}

func (c *Client) KillPane(ctx context.Context, paneID string) error {
	_, err := c.Cmd.Run(ctx, "kill-pane", "-t", paneID)
	return err
}

func (c *Client) KillWindow(ctx context.Context, windowName string) error {
	_, err := c.Cmd.Run(ctx, "kill-window", "-t", c.windowTarget(windowName))
	return err
}

// MovePane moves a pane into the named window via join-pane, creating
// the window with break-pane when it does not exist. Joins retry once
// after retiling, like splits.
func (c *Client) MovePane(ctx context.Context, paneID, windowName string) error {
	exists, err := c.WindowExists(ctx, windowName)
	if err != nil {
		return err
	}
	if !exists {
		_, err = c.Cmd.Run(ctx, "break-pane", "-d", "-s", paneID, "-n", windowName, "-t", c.SessionName+":")
		return err
	}
	target := c.windowTarget(windowName)
	out, err := c.Cmd.Run(ctx, "join-pane", "-d", "-s", paneID, "-t", target)
	if err != nil && strings.Contains(err.Error()+out, "no space for a new pane") {
		if _, layoutErr := c.Cmd.Run(ctx, "select-layout", "-t", target, "tiled"); layoutErr == nil {
			_, err = c.Cmd.Run(ctx, "join-pane", "-d", "-s", paneID, "-t", target)
		}
	}
	return err
}

func (c *Client) RenameWindow(ctx context.Context, target, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("window name is required")
	}
	resolved, err := c.resolveWindowTarget(ctx, target)
	if err != nil {
		return err
	}
	_, err = c.Cmd.Run(ctx, "rename-window", "-t", resolved, name)
	return err
}

func (c *Client) windowTarget(windowName string) string {
	return c.SessionName + ":" + windowName
}

func (c *Client) ListPanes(ctx context.Context) ([]PaneInfo, error) {
	format := "#{window_index}\t#{window_name}\t#{pane_id}\t#{pane_index}\t#{pane_current_command}\t#{pane_current_path}\t#{pane_active}\t#{pane_pid}\t#{@agency_role}\t#{@agency_parent}\t#{@agency_root}\t#{@agency_promotion}\t#{@agency_command}\t#{window_id}\t#{@agency_cloud}"
	out, err := c.Cmd.Run(ctx,
		"list-panes", "-a", "-s", "-t", c.SessionName, "-F", format,
	)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}

	var panes []PaneInfo
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) == 6 {
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
			continue
		}
		if len(parts) < 8 {
			continue
		}
		windowIdx, _ := strconv.Atoi(parts[0])
		idx, _ := strconv.Atoi(parts[3])
		pid, _ := strconv.Atoi(parts[7])
		pane := PaneInfo{
			ID:          parts[2],
			Index:       idx,
			WindowIndex: windowIdx,
			WindowName:  parts[1],
			Command:     parts[4],
			CWD:         parts[5],
			Active:      parts[6] == "1",
			PID:         pid,
		}
		if len(parts) >= 11 {
			pane.Role = parts[8]
			pane.ParentID = parts[9]
			pane.RootID = parts[10]
		}
		if len(parts) >= 12 {
			pane.PendingPromotion = parts[11]
		}
		if len(parts) >= 13 {
			pane.AgencyCommand = parts[12]
		}
		if len(parts) >= 14 {
			pane.WindowID = parts[13]
		}
		if len(parts) >= 15 {
			pane.CloudWindow = parts[14]
		}
		panes = append(panes, pane)
	}
	return panes, nil
}

func (c *Client) SelectLayoutForWindow(ctx context.Context, target, layout string) error {
	_, err := c.Cmd.Run(ctx, "select-layout", "-t", target, layout)
	return err
}

// WindowInfo holds the dimensions and pane count of a tmux window.
type WindowInfo struct {
	Width     int
	Height    int
	PaneCount int
}

func (c *Client) GetWindowInfoForWindow(ctx context.Context, target string) (WindowInfo, error) {
	out, err := c.Cmd.Run(ctx,
		"display-message", "-t", target, "-p",
		"#{window_width}\t#{window_height}\t#{window_panes}",
	)
	if err != nil {
		return WindowInfo{}, err
	}
	var info WindowInfo
	parts := strings.SplitN(out, "\t", 3)
	if len(parts) < 3 {
		return WindowInfo{}, fmt.Errorf("unexpected display-message output: %q", out)
	}
	info.Width, _ = strconv.Atoi(parts[0])
	info.Height, _ = strconv.Atoi(parts[1])
	info.PaneCount, _ = strconv.Atoi(parts[2])
	return info, nil
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
