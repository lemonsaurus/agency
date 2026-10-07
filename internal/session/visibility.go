package session

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/lemonsaurus/agency/internal/cloud"
)

// visibilityFormat lists each local pane with what decides whether anyone
// sees it: viewer panes carry @agency_cloud, and a window counts as seen when
// a client's session has it as its current window.
const visibilityFormat = "#{pane_id}\t#{@agency_cloud}\t#{window_active_clients}\t#{window_zoomed_flag}\t#{pane_active}"

// SetShownSink sets where the shown set goes: the link to the cloud host.
func (m *Manager) SetShownSink(sink func(line string)) {
	m.shownMu.Lock()
	defer m.shownMu.Unlock()
	m.shownSink = sink
}

// ShownLine is the last shown set sent to the host, for resending after a
// reconnect.
func (m *Manager) ShownLine() string {
	m.shownMu.Lock()
	defer m.shownMu.Unlock()
	return m.shownLine
}

// Visibility sends the host which viewers are on screen when that changed.
// The host parks the others: their remote clients stop receiving output
// until they show again.
func (m *Manager) Visibility(ctx context.Context) error {
	m.shownMu.Lock()
	defer m.shownMu.Unlock()
	if m.shownSink == nil {
		return nil
	}
	out, err := m.tmux.Cmd.Run(ctx, "list-panes", "-a", "-F", visibilityFormat)
	if err != nil {
		return err
	}
	device := cloud.Device(m.cfg.Session.Name)
	line := cloud.ShownLine(device, visibleViewers(device, out))
	if line == m.shownLine {
		return nil
	}
	m.shownLine = line
	m.shownSink(line)
	return nil
}

// visibleViewers returns the keys of the viewer panes on screen in out, a
// visibilityFormat listing. In a zoomed window only the active pane is seen.
func visibleViewers(device, out string) []string {
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 5 || parts[1] == "" || parts[1] == cloud.Placeholder {
			continue
		}
		if clients, _ := strconv.Atoi(parts[2]); clients == 0 {
			continue
		}
		if parts[3] == "1" && parts[4] != "1" {
			continue
		}
		seen[cloud.ViewerKey(device, parts[0])] = true
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
