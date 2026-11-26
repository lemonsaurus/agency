package tmux

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/lemonsaurus/agency/internal/order"
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

var commandTimeout = 10 * time.Second

// commandSlots bounds short-lived tmux clients across all clients in this process.
var commandSlots = make(chan struct{}, 8)

func (e *ExecCommander) Run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	select {
	case commandSlots <- struct{}{}:
		defer func() { <-commandSlots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	cmd := exec.CommandContext(ctx, e.bin(), e.args(args)...)
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
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
	TaskLabel        string `json:"taskLabel,omitempty"`
	Group            string `json:"group,omitempty"` // cloud agent's remote-world window
	Session          string `json:"session,omitempty"`
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
//
// A viewer key opts into parking: the client starts on its park session when
// its device does not show the key, and later moves between the two as the
// device reports what is on screen. Without a key the client always streams.
// A keyed viewer attaches only while its device holds the lease, and its
// view session comes back if anything removes it while the client stays.
func (c *Client) AttachWindow(ctx context.Context, windowID, viewer string) error {
	device, keyed := viewerDevice(viewer)
	if keyed {
		if lease := parseLease(c.showOption(ctx, leaseOption)); !lease.Holds(device) {
			return fmt.Errorf("in use on %s", lease.Host)
		}
	}
	c.SweepViews(ctx)
	id := os.Getpid()
	view := fmt.Sprintf("%s%d", ViewPrefix, id)
	park := fmt.Sprintf("%s%d", ParkPrefix, id)
	if _, err := c.Cmd.Run(ctx, "new-session", "-d", "-t", c.SessionName, "-s", view); err != nil {
		return err
	}
	defer c.Cmd.Run(ctx, "kill-session", "-t", "="+view)
	defer c.Cmd.Run(ctx, "kill-session", "-t", "="+park)
	if _, err := c.Cmd.Run(ctx, "select-window", "-t", view+":"+windowID); err != nil {
		return err
	}
	if !keyed {
		// destroy-unattached is set from inside the attached client, so a link
		// that dies before the deferred kill still takes the view session with it.
		return c.Cmd.Exec(ctx, "attach-session", "-t", view, ";", "set-option", "-t", view, "destroy-unattached", "on")
	}
	if err := c.markView(ctx, view, windowID, viewer); err != nil {
		return err
	}
	target := view
	if !c.isShown(ctx, viewer, device) && c.park(ctx, park, windowID) == nil {
		target = park
	}
	heal, stop := context.WithCancel(ctx)
	defer stop()
	go c.keepView(heal, view, windowID, viewer)
	return c.Cmd.Exec(ctx, "attach-session", "-t", "="+target)
}

// markView tags a view session with its viewer key and window, which the
// host's reconcile reads.
func (c *Client) markView(ctx context.Context, view, windowID, viewer string) error {
	for _, option := range [][2]string{{"@agency_viewer", viewer}, {"@agency_window", windowID}} {
		if _, err := c.Cmd.Run(ctx, "set-option", "-t", "="+view+":", option[0], option[1]); err != nil {
			return err
		}
	}
	return nil
}

// viewCheck is how often an attached viewer checks that its view session
// still exists. A parked client whose view session is gone is invisible to
// reconcile and would sit on its park screen for good.
var viewCheck = 15 * time.Second

// keepView recreates the view session whenever it goes missing while the
// client stays attached, until ctx ends.
func (c *Client) keepView(ctx context.Context, view, windowID, viewer string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(viewCheck + time.Duration(rand.Int64N(int64(viewCheck/2)))):
		}
		if _, err := c.Cmd.Run(ctx, "has-session", "-t", "="+view); err == nil || ctx.Err() != nil {
			continue
		}
		if _, err := c.Cmd.Run(ctx, "new-session", "-d", "-t", c.SessionName, "-s", view); err != nil {
			continue
		}
		if _, err := c.Cmd.Run(ctx, "select-window", "-t", view+":"+windowID); err == nil {
			_ = c.markView(ctx, view, windowID, viewer)
		}
	}
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

// NewSessionWindow creates the session with a named first window running command.
func (c *Client) NewSessionWindow(ctx context.Context, name, command string) (string, error) {
	out, err := c.Cmd.Run(ctx, "new-session", "-d", "-s", c.SessionName, "-n", name, "-P", "-F", "#{pane_id}", command)
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

// WindowID returns the id of the session's window with this name, or "" when
// there is none. Ids stay valid targets for names containing '.' or ':'.
func (c *Client) WindowID(ctx context.Context, name string) (string, error) {
	windows, err := c.listWindowRefs(ctx)
	for _, window := range windows {
		if window.Name == name {
			return window.ID, nil
		}
	}
	return "", err
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

// PasteText pastes text into a pane as one bracketed paste, so the program
// treats it as a paste rather than typed keys, and optionally presses Enter.
func (c *Client) PasteText(ctx context.Context, paneID, text string, enter bool) error {
	buffer := "agency-paste-" + strings.TrimPrefix(paneID, "%")
	if _, err := c.Cmd.Run(ctx, "set-buffer", "-b", buffer, "--", text, ";", "paste-buffer", "-p", "-d", "-b", buffer, "-t", paneID); err != nil {
		return err
	}
	if enter {
		return c.SendText(ctx, paneID, "", true)
	}
	return nil
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

// MoveWindow places a window at the given index. An occupied index shifts
// that window and everything after it; renumber-windows closes the gap.
func (c *Client) MoveWindow(ctx context.Context, target string, index int) error {
	resolved, err := c.resolveWindowTarget(ctx, target)
	if err != nil {
		return err
	}
	windows, err := c.listWindowRefs(ctx)
	if err != nil {
		return err
	}
	current := -1
	var occupant *windowRef
	for i := range windows {
		if windows[i].ID == resolved {
			current = windows[i].Index
		}
		if windows[i].Index == index {
			occupant = &windows[i]
		}
	}
	if current == index {
		return nil
	}
	if occupant == nil {
		_, err = c.Cmd.Run(ctx, "move-window", "-s", resolved, "-t", c.SessionName+":"+strconv.Itoa(index))
		return err
	}
	side := "-b"
	if current >= 0 && current < index {
		side = "-a"
	}
	_, err = c.Cmd.Run(ctx, "move-window", side, "-s", resolved, "-t", occupant.ID)
	return err
}

// ArrangeWindows puts the session's windows in the order of ids with the
// fewest moves, numbers them without gaps, and returns how many moves it
// made. Targets name the session, so sessions grouped with it follow and
// never take the move themselves.
func (c *Client) ArrangeWindows(ctx context.Context, ids []string) (int, error) {
	windows, err := c.listWindowRefs(ctx)
	if err != nil {
		return 0, err
	}
	want, _, err := c.groupWindows(ctx)
	if err != nil {
		return 0, err
	}
	defer c.showWindows(ctx, want)
	current := make([]string, len(windows))
	for i, window := range windows {
		current[i] = window.ID
	}
	made := 0
	for _, move := range order.Moves(current, ids) {
		args := []string{"move-window", "-a", "-s", c.SessionName + ":" + move.Name, "-t", c.SessionName + ":" + move.After}
		if move.After == "" {
			if current[0] == move.Name {
				continue
			}
			args[1], args[5] = "-b", c.SessionName+":"+current[0]
		}
		if _, err := c.Cmd.Run(ctx, args...); err != nil {
			return 0, err
		}
		current = order.Apply(current, []order.Move{move})
		made++
	}
	if windows, err = c.listWindowRefs(ctx); err != nil {
		return made, err
	}
	for i, window := range windows {
		if window.Index != windows[0].Index+i {
			_, err = c.Cmd.Run(ctx, "move-window", "-r", "-t", c.SessionName)
			return made, err
		}
	}
	return made, nil
}

// groupWindows maps the session and every session grouped with it to the
// window it should show, a view session's own window or else the one shown
// now, and to the window it shows now. Grouped sessions keep a window index,
// not a window, so a move or renumber changes what they show.
func (c *Client) groupWindows(ctx context.Context) (want, now map[string]string, err error) {
	out, err := c.Cmd.Run(ctx, "list-sessions", "-F", "#{session_name}\t#{session_group}\t#{window_id}\t#{@agency_window}")
	if err != nil {
		return nil, nil, err
	}
	var sessions [][]string
	group := ""
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			continue
		}
		if parts[0] == c.SessionName {
			group = parts[1]
		}
		sessions = append(sessions, parts)
	}
	want, now = map[string]string{}, map[string]string{}
	for _, parts := range sessions {
		if parts[0] == c.SessionName || group != "" && parts[1] == group {
			want[parts[0]], now[parts[0]] = cmp.Or(parts[3], parts[2]), parts[2]
		}
	}
	return want, now, nil
}

// showWindows puts each session back on its window from groupWindows.
func (c *Client) showWindows(ctx context.Context, want map[string]string) {
	_, now, err := c.groupWindows(ctx)
	if err != nil {
		return
	}
	for session, window := range want {
		if current, ok := now[session]; ok && current != window {
			_, _ = c.Cmd.Run(ctx, "select-window", "-t", "="+session+":"+window)
		}
	}
}

func (c *Client) windowTarget(windowName string) string {
	return c.SessionName + ":" + windowName
}

// PanePIDs lists every pane's ID and process ID in ListPanes order, leaving
// out parked viewers like ListPanes does. Three formats instead of eighteen
// keep requester lookups cheap for a busy tmux server.
func (c *Client) PanePIDs(ctx context.Context) ([]PaneInfo, error) {
	out, err := c.Cmd.Run(ctx, "list-panes", "-a", "-F", "#{pane_pid}\t#{pane_id}\t#{session_name}")
	if err != nil {
		return nil, err
	}
	var panes []PaneInfo
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || strings.HasPrefix(parts[2], ParkPrefix) {
			continue
		}
		if pid, err := strconv.Atoi(parts[0]); err == nil {
			panes = append(panes, PaneInfo{ID: parts[1], PID: pid, Session: parts[2]})
		}
	}
	return panes, nil
}

func (c *Client) ListPanes(ctx context.Context) ([]PaneInfo, error) {
	format := "#{window_index}\t#{window_name}\t#{pane_id}\t#{pane_index}\t#{pane_current_command}\t#{pane_current_path}\t#{pane_active}\t#{pane_pid}\t#{@agency_role}\t#{@agency_parent}\t#{@agency_root}\t#{@agency_promotion}\t#{@agency_command}\t#{window_id}\t#{@agency_cloud}\t#{@agency_task_label}\t#{@agency_group}\t#{session_name}"
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
		if len(parts) >= 16 {
			pane.TaskLabel = parts[15]
		}
		if len(parts) >= 18 {
			pane.Group = parts[16]
			pane.Session = parts[17]
		}
		if strings.HasPrefix(pane.Session, ParkPrefix) {
			continue
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
