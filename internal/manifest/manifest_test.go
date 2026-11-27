package manifest

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/lemonsaurus/agency/internal/tmux"
)

func b64(s string) string { return base64.RawStdEncoding.EncodeToString([]byte(s)) }

// The manifest holds the box's own panes in window order: viewer sessions
// that repeat a window add nothing, and a pane keeps its role, parent, label,
// group and the command it was started with.
func TestBuildKeepsTheBoxPanesInWindowOrder(t *testing.T) {
	panes := []tmux.PaneInfo{
		{ID: "%2", Session: "cloud", WindowID: "@2", WindowIndex: 2, WindowName: "pi@b", Group: "beta", Role: "manager", ParentID: "%1", RootID: "%1", TaskLabel: "Kid", AgencyCommand: b64("pi --session /s/2.jsonl"), CWD: "/w/b"},
		{ID: "%1", Session: "cloud", WindowID: "@1", WindowIndex: 1, WindowName: "pi@a", Group: "alpha", Role: "controller", RootID: "%1", TaskLabel: "Boss", AgencyCommand: b64("pi"), CWD: "/w/a"},
		{ID: "%1", Session: "view-9", WindowID: "@1", WindowIndex: 1},
		{ID: "%2", Session: "park-4", WindowID: "@2", WindowIndex: 2},
	}
	got := Build("cloud", panes, func(id, command string) string { return "file-" + id })
	if len(got.Windows) != 2 || got.Windows[0].Group != "alpha" || got.Windows[1].Group != "beta" {
		t.Fatalf("windows = %+v", got.Windows)
	}
	kid := got.Windows[1].Panes[0]
	if kid.ID != "%2" || kid.Parent != "%1" || kid.Label != "Kid" || kid.Command != "pi --session /s/2.jsonl" || kid.Session != "file-%2" || kid.Cwd != "/w/b" {
		t.Fatalf("kid = %+v", kid)
	}
}

func TestSaveIsAtomicAndLoadReadsItBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "manifest.json")
	if m, err := Load(path); err != nil || len(m.Panes()) != 0 {
		t.Fatalf("missing file = %+v, %v", m, err)
	}
	want := Manifest{Version: Version, Session: "cloud", Server: "1:2", Windows: []Window{{Name: "w", Panes: []Pane{{ID: "%1", Command: "sh"}}}}}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || !Same(got, want) {
		t.Fatalf("loaded %+v, %v", got, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	if err := Save(path, Manifest{Version: Version + 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a newer manifest was accepted")
	}
}

// A manifest from a server that is gone becomes a pending restore once; a
// manifest of the running server, or one with no panes, does not, and
// finishing a restore marks the manifest as the new server's.
func TestFreezeStartsARestoreOnlyForADeadServer(t *testing.T) {
	dir := t.TempDir()
	path, pending := filepath.Join(dir, "m.json"), filepath.Join(dir, "p.json")
	saved := Manifest{Version: Version, Server: "100:1", Windows: []Window{{Name: "w", Panes: []Pane{{ID: "%1", Role: "worker", Command: "sh"}}}}}
	if err := Save(path, saved); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Freeze(path, pending, "100:1", false); ok {
		t.Fatal("the running server's manifest was frozen")
	}
	if _, ok, _ := Freeze(path, pending, "200:2", true); !ok {
		t.Fatal("a dead server's manifest was not found")
	}
	if _, err := os.Stat(pending); err == nil {
		t.Fatal("a dry run wrote the pending file")
	}
	state, ok, err := Freeze(path, pending, "200:2", false)
	if !ok || err != nil || len(state.Manifest.Panes()) != 1 {
		t.Fatalf("freeze = %+v, %v, %v", state, ok, err)
	}
	state.Restored["%1"] = "%7"
	if err := Save(pending, state); err != nil {
		t.Fatal(err)
	}
	again, ok, _ := Freeze(path, pending, "300:3", false)
	if !ok || again.Restored["%1"] != "%7" {
		t.Fatalf("a restore in progress was not resumed: %+v", again)
	}
	if err := Finish(path, pending, "200:2"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Freeze(path, pending, "200:2", false); ok {
		t.Fatal("a finished restore was frozen again")
	}
}

type fakeSource struct{ panes []tmux.PaneInfo }

func (f fakeSource) ListPanes(context.Context) ([]tmux.PaneInfo, error) { return f.panes, nil }
func (f fakeSource) ServerID(context.Context) (string, error)           { return "1:1", nil }

// The writer leaves the dead session's manifest alone while a restore is pending.
func TestWriterPausesWhileARestoreIsPending(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Path: filepath.Join(dir, "m.json"), PendingPath: filepath.Join(dir, "p.json"), Session: "cloud",
		Source: fakeSource{[]tmux.PaneInfo{{ID: "%1", Session: "cloud", WindowID: "@1", Role: "worker"}}}}
	if err := os.WriteFile(w.PendingPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path); err == nil {
		t.Fatal("wrote while a restore was pending")
	}
	os.Remove(w.PendingPath)
	if err := w.Write(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, _ := Load(w.Path); len(m.Panes()) != 1 || m.Server != "1:1" {
		t.Fatalf("manifest = %+v", m)
	}
}
