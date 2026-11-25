package session

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/order"
)

// skyWindow is one window of a world, named after the group it shows.
type skyWindow struct {
	id, group string
}

func windowIDs(windows []skyWindow) []string {
	ids := make([]string, len(windows))
	for i, window := range windows {
		ids[i] = window.id
	}
	return ids
}

func groupNames(windows []skyWindow) []string {
	names := make([]string, len(windows))
	for i, window := range windows {
		names[i] = window.group
	}
	return names
}

// WindowOrder applies moves to the box's window order and returns the order,
// with every window arranged to it. Devices call it over SSH on every sync.
func (m *Manager) WindowOrder(ctx context.Context, moves []order.Move) ([]string, error) {
	if !m.WindowPerPane {
		return nil, fmt.Errorf("window order lives on the sky box")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.boxOrder(ctx, func(names []string) []string { return order.Apply(names, moves) })
}

// boxWindows lists the box's windows in order with each one's group. View
// sessions repeat the windows and park sessions hold none of them.
func (m *Manager) boxWindows(ctx context.Context) ([]skyWindow, error) {
	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return nil, err
	}
	var windows []skyWindow
	for _, pane := range panes {
		if pane.Session != m.tmux.SessionName || slices.ContainsFunc(windows, func(w skyWindow) bool { return w.id == pane.WindowID }) {
			continue
		}
		windows = append(windows, skyWindow{id: pane.WindowID, group: cloud.Group(pane)})
	}
	return windows, nil
}

// boxOrder reconciles the saved order with the groups open now, applies
// change, saves, and arranges the box's windows to the result. Callers hold
// m.mu.
func (m *Manager) boxOrder(ctx context.Context, change func([]string) []string) ([]string, error) {
	if m.Order == nil {
		return nil, fmt.Errorf("no window order on this server")
	}
	windows, err := m.boxWindows(ctx)
	if err != nil {
		return nil, err
	}
	present := groupNames(windows)
	names, err := m.Order.Update(func(names []string) []string {
		names = order.Reconcile(names, present)
		if change != nil {
			names = order.Reconcile(change(names), present)
		}
		return names
	})
	if err != nil {
		return nil, err
	}
	sorted := order.Rank(windows, names, func(w skyWindow) string { return w.group })
	if _, err := m.tmux.ArrangeWindows(ctx, windowIDs(sorted)); err != nil {
		return nil, err
	}
	return names, nil
}

// moveGroup places a box window at index, counted from 1 as the devices'
// status bars show it. The target is a group, a window or pane id inside
// one, or the index a group shows at.
func (m *Manager) moveGroup(ctx context.Context, target string, index int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	names, err := m.boxOrder(ctx, nil)
	if err != nil {
		return err
	}
	group := ""
	if slices.Contains(names, target) {
		group = target
	} else if panes, err := m.tmux.ListPanes(ctx); err == nil {
		for _, pane := range panes {
			if pane.ID == target || pane.WindowID == target {
				group = cloud.Group(pane)
				break
			}
		}
	}
	if at, err := strconv.Atoi(target); group == "" && err == nil && at >= 1 && at <= len(names) {
		group = names[at-1]
	}
	if group == "" {
		return fmt.Errorf("window %q not found", target)
	}
	_, err = m.boxOrder(ctx, func(names []string) []string { return order.Place(names, group, index-1) })
	return err
}

// skyWindows lists this device's sky windows in order. A window is named
// after the group its viewers show, so a window renamed in tmux alone keeps
// its place; a window whose viewers the device has not synced yet goes by
// its name.
func (m *Manager) skyWindows(ctx context.Context) ([]skyWindow, error) {
	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return nil, err
	}
	var windows []skyWindow
	groups := map[string][]string{}
	for _, pane := range panes {
		if pane.Session != m.remote.SessionName {
			continue
		}
		if !slices.ContainsFunc(windows, func(w skyWindow) bool { return w.id == pane.WindowID }) {
			windows = append(windows, skyWindow{id: pane.WindowID, group: pane.WindowName})
		}
		if group, ok := m.skyGroups[pane.CloudWindow]; ok {
			groups[pane.WindowID] = append(groups[pane.WindowID], group)
		}
	}
	for i, window := range windows {
		if shown := groups[window.id]; len(shown) > 0 && !slices.Contains(shown, window.group) {
			windows[i].group = shown[0]
		}
	}
	return windows, nil
}

// skyMoves are the moves made on this device since it last arranged its sky
// windows to the box's order. Windows opened or closed since are not moves.
func (m *Manager) skyMoves(ctx context.Context) []order.Move {
	if m.skyBase == nil {
		return nil
	}
	windows, err := m.skyWindows(ctx)
	if err != nil {
		return nil
	}
	return order.Moves(m.skyBase, groupNames(windows))
}

// arrangeSky sends the box this device's moves, takes back the box's order,
// and arranges the sky windows to it. Callers hold m.mu.
func (m *Manager) arrangeSky(ctx context.Context, moves []order.Move) error {
	payload, _ := json.Marshal(moves)
	out, err := m.cloud.Run(ctx, 15*time.Second, "window-order", string(payload))
	if err != nil {
		return err
	}
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		return fmt.Errorf("parsing sky window order: %w", err)
	}
	windows, err := m.skyWindows(ctx)
	if err != nil {
		return err
	}
	if len(windows) > 0 {
		sorted := order.Rank(windows, names, func(w skyWindow) string { return w.group })
		if _, err := m.remote.ArrangeWindows(ctx, windowIDs(sorted)); err != nil {
			return err
		}
	}
	m.skyBase = names
	return nil
}

// WindowMoved sends the box a move Lemon made in this device's sky world,
// by tmux keys, menu, or command, and arranges the windows to the order the
// box answers with. tmux hooks call it on every window link.
func (m *Manager) WindowMoved(ctx context.Context) error {
	if m.cfg.Cloud.Host == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Link != nil && m.Link.Collapsed() {
		return nil
	}
	moves := m.skyMoves(ctx)
	if len(moves) == 0 {
		return nil
	}
	return m.arrangeSky(ctx, moves)
}
