// Package restore rebuilds a dead session from its manifest: panes in
// parent-first order, Pi panes resumed on their conversations, and every
// resumed agent told its new ID and its relatives'.
package restore

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/manifest"
)

// Step is one pane to bring back.
type Step struct {
	Pane manifest.Pane
	// Window is the group or window name the pane returns to.
	Window string
	// Command is what the pane runs: the saved command, or a Pi resume.
	Command string
	// Resumed is whether Command continues a Pi conversation.
	Resumed bool
	// Skip says why the pane is not restored; empty restores it.
	Skip string
}

// Options narrow a plan.
type Options struct {
	// Skip are pane IDs and label fragments to leave dead.
	Skip []string
}

// Plan lists the manifest's panes parent first, windows in order within each
// generation. A pane without an agency role is not an agent and stays out.
func Plan(m manifest.Manifest, opts Options) []Step {
	var steps []Step
	byID := map[string]int{}
	for _, window := range m.Windows {
		name := window.Group
		if name == "" {
			name = window.Name
		}
		for _, pane := range window.Panes {
			step := Step{Pane: pane, Window: name, Command: pane.Command}
			switch {
			case window.Name == "control" && pane.Label == "" && isShell(pane.Command):
				step.Skip = "control window"
			case pane.Role == "":
				step.Skip = "not an agent"
			case skipped(pane, opts.Skip):
				step.Skip = "skipped"
			}
			if step.Skip == "" && pane.Session != "" && isPi(pane.Command) {
				step.Command, step.Resumed = ResumeCommand(pane.Command, pane.Session), true
			}
			byID[pane.ID] = len(steps)
			steps = append(steps, step)
		}
	}
	depth := make([]int, len(steps))
	for i := range steps {
		for id, seen := steps[i].Pane.Parent, 0; id != "" && seen < len(steps); seen++ {
			parent, ok := byID[id]
			if !ok || steps[parent].Skip != "" {
				break
			}
			depth[i]++
			id = steps[parent].Pane.Parent
		}
	}
	order := make([]int, len(steps))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return depth[order[a]] < depth[order[b]] })
	sorted := make([]Step, len(steps))
	for i, at := range order {
		sorted[i] = steps[at]
	}
	return sorted
}

func skipped(pane manifest.Pane, skip []string) bool {
	for _, item := range skip {
		if item == pane.ID || item != "" && !strings.HasPrefix(item, "%") && strings.Contains(strings.ToLower(pane.Label), strings.ToLower(item)) {
			return true
		}
	}
	return false
}

// isShell is whether command is a bare login shell: the control window's own, which a new
// session already has.
func isShell(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return true
	}
	switch filepath.Base(fields[0]) {
	case "zsh", "bash", "sh", "fish", "dash":
		return len(fields) == 1
	}
	return false
}

func isPi(command string) bool {
	fields := strings.Fields(command)
	return len(fields) > 0 && (fields[0] == "pi" || strings.HasSuffix(fields[0], "/pi"))
}

// Spec is what the daemon is asked to start for a step, given the new IDs
// restored so far.
func (s Step) Spec(restored map[string]string) control.RestoreSpec {
	return control.RestoreSpec{
		Old:     s.Pane.ID,
		Role:    control.Role(s.Pane.Role),
		Parent:  restored[s.Pane.Parent],
		Root:    restored[s.Pane.Root],
		Label:   s.Pane.Label,
		Window:  s.Window,
		Command: s.Command,
		Dir:     s.Pane.Cwd,
	}
}

// Note is the recovery message for a restored pane: its old and new IDs and
// its parent's and children's new IDs.
func Note(m manifest.Manifest, old string, restored map[string]string) string {
	byID := map[string]manifest.Pane{}
	for _, pane := range m.Panes() {
		byID[pane.ID] = pane
	}
	pane := byID[old]
	var b strings.Builder
	fmt.Fprintf(&b, "Recovery note: the sky tmux server crashed and your pane died with it. You were %s; you are now %s. ", old, restored[old])
	if parent, ok := restored[pane.Parent]; ok {
		fmt.Fprintf(&b, "Your parent is now %s (%s). ", parent, byID[pane.Parent].Label)
	} else {
		b.WriteString("You have no parent pane; report to Lemon. ")
	}
	var children []string
	for _, window := range m.Windows {
		for _, child := range window.Panes {
			if child.Parent == old && restored[child.ID] != "" {
				children = append(children, fmt.Sprintf("%s -> %s (%s)", child.ID, restored[child.ID], child.Label))
			}
		}
	}
	if len(children) > 0 {
		fmt.Fprintf(&b, "Your children: %s. ", strings.Join(children, "; "))
	}
	b.WriteString("Every other pane ID in your history is dead. Processes you started from your old pane may still be running, so check before starting duplicates. Re-check git status, worktrees and PR state before acting, then send your parent one message with where you stand and what you're waiting on.")
	return b.String()
}
