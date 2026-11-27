// Package manifest is the durable record of every pane in the session: what
// `agency restore` rebuilds a dead tmux server from. The daemon rewrites it
// atomically whenever a pane changes.
package manifest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lemonsaurus/agency/internal/tmux"
)

// Version is the file format; a reader refuses a newer one.
const Version = 1

// Pane is one agent or process in the session.
type Pane struct {
	ID        string `json:"id"`
	Role      string `json:"role,omitempty"`
	Parent    string `json:"parent,omitempty"`
	Root      string `json:"root,omitempty"`
	Label     string `json:"label,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Command   string `json:"command"`
	Cwd       string `json:"cwd,omitempty"`
	Promotion string `json:"promotion,omitempty"`
	// Session is the Pi session file a Pi pane resumes.
	Session string `json:"session,omitempty"`
}

// Window is one tmux window, with the panes in it from first to last. Group
// is the sky window group the window shows; Name is the tmux window name.
type Window struct {
	Name  string `json:"name"`
	Group string `json:"group,omitempty"`
	Panes []Pane `json:"panes"`
}

// Manifest is the whole session, windows in order.
type Manifest struct {
	Version int      `json:"version"`
	Session string   `json:"session"`
	Windows []Window `json:"windows"`
	// Server identifies the tmux server the pane IDs belong to.
	Server  string    `json:"server,omitempty"`
	SavedAt time.Time `json:"savedAt"`
}

// Panes lists every pane in window order.
func (m Manifest) Panes() []Pane {
	var panes []Pane
	for _, window := range m.Windows {
		panes = append(panes, window.Panes...)
	}
	return panes
}

// Pending is a restore in progress: the manifest being rebuilt and the new ID
// of every pane brought back so far. A rerun picks up where it stopped.
type Pending struct {
	Manifest Manifest          `json:"manifest"`
	Restored map[string]string `json:"restored"`
	// Noted are the old IDs of panes that have their recovery note.
	Noted   map[string]bool `json:"noted,omitempty"`
	Started time.Time       `json:"started"`
}

// Build turns the box's tmux panes into a manifest. sessionFile finds the Pi
// session file of a pane from its ID and command; it may be nil.
func Build(session string, panes []tmux.PaneInfo, sessionFile func(paneID, command string) string) Manifest {
	var own []tmux.PaneInfo
	seen := map[string]bool{}
	for _, pane := range panes {
		if pane.Session != session || seen[pane.ID] {
			continue
		}
		seen[pane.ID] = true
		own = append(own, pane)
	}
	sort.SliceStable(own, func(i, j int) bool {
		if own[i].WindowIndex != own[j].WindowIndex {
			return own[i].WindowIndex < own[j].WindowIndex
		}
		return own[i].Index < own[j].Index
	})
	manifest := Manifest{Version: Version, Session: session}
	index := map[string]int{}
	for _, pane := range own {
		at, ok := index[pane.WindowID]
		if !ok {
			at = len(manifest.Windows)
			index[pane.WindowID] = at
			manifest.Windows = append(manifest.Windows, Window{Name: pane.WindowName, Group: pane.Group})
		}
		command, _ := base64.RawStdEncoding.DecodeString(pane.AgencyCommand)
		entry := Pane{
			ID: pane.ID, Role: pane.Role, Parent: pane.ParentID, Root: pane.RootID, Label: pane.TaskLabel,
			Command: string(command), Cwd: pane.CWD,
		}
		if entry.Command == "" {
			entry.Command = pane.Command
		}
		if promotion, err := base64.RawStdEncoding.DecodeString(pane.PendingPromotion); err == nil {
			entry.Promotion = string(promotion)
		}
		if sessionFile != nil {
			entry.Session = sessionFile(pane.ID, entry.Command)
		}
		manifest.Windows[at].Panes = append(manifest.Windows[at].Panes, entry)
	}
	return manifest
}

// Same is whether two manifests describe the same panes, ignoring when they
// were saved.
func Same(a, b Manifest) bool {
	a.SavedAt, b.SavedAt = time.Time{}, time.Time{}
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

// Save writes v as JSON to path atomically: a reader sees the old file or the
// new one, and a crash mid-write leaves the old one.
func Save(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Load reads a manifest; a missing file is an empty manifest.
func Load(path string) (Manifest, error) {
	var manifest Manifest
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return manifest, nil
	}
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Version > Version {
		return Manifest{}, errors.New("manifest is from a newer agency")
	}
	return manifest, nil
}

// LoadPending reads a restore in progress; ok is false when there is none.
func LoadPending(path string) (pending Pending, ok bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pending, false, nil
	}
	if err != nil {
		return pending, false, err
	}
	if err := json.Unmarshal(data, &pending); err != nil {
		return pending, false, err
	}
	if pending.Restored == nil {
		pending.Restored = map[string]string{}
	}
	return pending, true, nil
}
