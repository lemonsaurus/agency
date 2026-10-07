package tmux

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const (
	// ViewPrefix names the private session of each attached viewer: view-<pid
	// of the agency process that runs its tmux client>.
	ViewPrefix = "view-"
	// ParkPrefix names the session a hidden viewer's client sits on, one per
	// view session: park-<same pid>.
	ParkPrefix = "park-"
)

// ViewState counts one device's viewers after a reconcile.
type ViewState struct {
	Views int // viewer sessions of the device
	Shown int // of those, on screen
	Moved int // clients switched between their view and park session
}

// shownOption is the server option holding the viewer keys a device has on
// screen: "=" then space-separated keys. Unset means the device never reported
// and every viewer streams.
func shownOption(device string) string {
	return "@agency_shown_" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			return r
		}
		return '_'
	}, device)
}

// viewerDevice returns the device a viewer key belongs to.
func viewerDevice(key string) (string, bool) {
	i := strings.LastIndex(key, "/")
	if i <= 0 {
		return "", false
	}
	return key[:i], true
}

func parseShown(value string) (keys []string, set bool) {
	rest, ok := strings.CutPrefix(value, "=")
	if !ok {
		return nil, false
	}
	return strings.Fields(rest), true
}

func (c *Client) showOption(ctx context.Context, name string) string {
	out, err := c.Cmd.Run(ctx, "show-options", "-gqv", name)
	if err != nil {
		return ""
	}
	return out
}

// isShown reports whether the viewer's device has it on screen.
func (c *Client) isShown(ctx context.Context, viewer, device string) bool {
	keys, set := parseShown(c.showOption(ctx, shownOption(device)))
	return !set || slices.Contains(keys, viewer)
}

func parkCommand(windowID string) string {
	bin, err := os.Executable()
	if err != nil {
		bin = "agency"
	}
	return fmt.Sprintf("%s cloud park-screen %s", bin, windowID)
}

// CaptureWindow returns the visible text of a window's active pane, taking
// the alternate screen when the program runs on it.
func (c *Client) CaptureWindow(ctx context.Context, windowID string) string {
	for _, extra := range [][]string{{"-a", "-q"}, nil} {
		args := append([]string{"capture-pane", "-p", "-t", windowID}, extra...)
		if out, err := c.Cmd.Run(ctx, args...); err == nil && strings.TrimSpace(out) != "" {
			return out
		}
	}
	return ""
}

// park makes sure the session a hidden viewer sits on exists and shows the
// window's text as it is now.
func (c *Client) park(ctx context.Context, name, windowID string) error {
	command := parkCommand(windowID)
	if _, err := c.Cmd.Run(ctx, "has-session", "-t", "="+name); err != nil {
		_, err = c.Cmd.Run(ctx, "new-session", "-d", "-s", name, "-x", "200", "-y", "50", command)
		return err
	}
	_, err := c.Cmd.Run(ctx, "respawn-pane", "-k", "-t", "="+name+":", command)
	return err
}

type viewSession struct {
	name, key, window string
	attached          int
}

func (c *Client) viewSessions(ctx context.Context) ([]viewSession, error) {
	out, err := c.Cmd.Run(ctx, "list-sessions", "-F", "#{session_name}\t#{@agency_viewer}\t#{@agency_window}\t#{session_attached}")
	if err != nil {
		return nil, err
	}
	var sessions []viewSession
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			continue
		}
		attached, _ := strconv.Atoi(parts[3])
		sessions = append(sessions, viewSession{name: parts[0], key: parts[1], window: parts[2], attached: attached})
	}
	return sessions, nil
}

// clientSessions maps each session to the ttys of its attached clients.
func (c *Client) clientSessions(ctx context.Context) (map[string][]string, error) {
	out, err := c.Cmd.Run(ctx, "list-clients", "-F", "#{client_tty}\t#{client_session}")
	if err != nil {
		return nil, err
	}
	clients := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		if tty, session, ok := strings.Cut(line, "\t"); ok {
			clients[session] = append(clients[session], tty)
		}
	}
	return clients, nil
}

// ApplyShown records the viewer keys device has on screen and reconciles.
func (c *Client) ApplyShown(ctx context.Context, device string, keys []string) (ViewState, error) {
	if _, err := c.Cmd.Run(ctx, "set-option", "-g", shownOption(device), "="+strings.Join(keys, " ")); err != nil {
		return ViewState{}, err
	}
	return c.Reconcile(ctx, device)
}

// Reconcile moves each of device's viewer clients onto its view session when
// the device shows it and onto its park session when it does not. Shows go
// first so a window swap never leaves a gap. A device that never reported is
// left alone.
func (c *Client) Reconcile(ctx context.Context, device string) (ViewState, error) {
	var state ViewState
	keys, set := parseShown(c.showOption(ctx, shownOption(device)))
	if !set {
		return state, nil
	}
	sessions, err := c.viewSessions(ctx)
	if err != nil {
		return state, err
	}
	clients, err := c.clientSessions(ctx)
	if err != nil {
		return state, err
	}
	type move struct{ tty, to, park, window string }
	var shows, hides []move
	prefix := device + "/"
	for _, s := range sessions {
		if !strings.HasPrefix(s.key, prefix) || !strings.HasPrefix(s.name, ViewPrefix) {
			continue
		}
		park := ParkPrefix + strings.TrimPrefix(s.name, ViewPrefix)
		state.Views++
		if slices.Contains(keys, s.key) {
			state.Shown++
			for _, tty := range clients[park] {
				shows = append(shows, move{tty: tty, to: s.name})
			}
			continue
		}
		for _, tty := range clients[s.name] {
			hides = append(hides, move{tty: tty, to: park, park: park, window: s.window})
		}
	}
	for _, m := range shows {
		if _, err := c.Cmd.Run(ctx, "switch-client", "-c", m.tty, "-t", "="+m.to); err != nil {
			return state, err
		}
		state.Moved++
	}
	for _, m := range hides {
		if err := c.park(ctx, m.park, m.window); err != nil {
			return state, err
		}
		if _, err := c.Cmd.Run(ctx, "switch-client", "-c", m.tty, "-t", "="+m.to); err != nil {
			return state, err
		}
		state.Moved++
	}
	return state, nil
}

// SweepViews kills view and park sessions whose agency process is gone and
// that no client uses. A view and its park session count as one.
func (c *Client) SweepViews(ctx context.Context) int {
	sessions, err := c.viewSessions(ctx)
	if err != nil {
		return 0
	}
	attached := map[int]int{}
	for _, s := range sessions {
		if pid, ok := viewPID(s.name); ok {
			attached[pid] += s.attached
		}
	}
	swept := 0
	for _, s := range sessions {
		pid, ok := viewPID(s.name)
		if !ok || attached[pid] > 0 || processAlive(pid) {
			continue
		}
		if _, err := c.Cmd.Run(ctx, "kill-session", "-t", "="+s.name); err == nil {
			swept++
		}
	}
	return swept
}

func viewPID(session string) (int, bool) {
	for _, prefix := range []string{ViewPrefix, ParkPrefix} {
		if rest, ok := strings.CutPrefix(session, prefix); ok {
			pid, err := strconv.Atoi(rest)
			return pid, err == nil && pid > 0
		}
	}
	return 0, false
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
