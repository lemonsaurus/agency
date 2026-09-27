package live

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// The box's activity diary: the dotagents agency extension in every Pi pane appends one JSON line
// to activity.jsonl when the pane gets a prompt, narrates a step (at most once a minute) and finishes.
type diaryEntry struct {
	At    int64  `json:"at"`
	Pane  string `json:"pane"`
	Label string `json:"label"`
	Dir   string `json:"cwd"`
	Kind  string `json:"kind"` // asked, step, answered, stopped, failed
	Text  string `json:"text"`
}

type ActivityEntry struct {
	When string `json:"when"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// SessionActivity is one session's recent diary. State is working, idle, stopped or failed, from the
// last entry; open sessions with no entries in the window fall back to their transcript.
type SessionActivity struct {
	Session      string          `json:"session"`
	PaneID       string          `json:"pane_id,omitempty"`
	Project      string          `json:"project"`
	Open         bool            `json:"open"`
	State        string          `json:"state"`
	LastActivity string          `json:"last_activity,omitempty"`
	Entries      []ActivityEntry `json:"entries,omitempty"`
	LastAnswer   string          `json:"last_answer,omitempty"`
	at           int64
}

const (
	diaryTail       = 512 * 1024
	entriesPerPane  = 6
	activityWindow  = 12 * time.Hour
	diaryAnswerSize = 400
)

// Activity is what every session has been doing in the window, most recently active first.
func Activity(ctx context.Context, box Box, path string, now time.Time, window time.Duration) ([]SessionActivity, error) {
	panes, err := box.Panes(ctx)
	if err != nil {
		return nil, err
	}
	entries := readDiary(path, now.Add(-window).UnixMilli())
	byPane := map[string]*SessionActivity{}
	var order []*SessionActivity
	for _, entry := range entries {
		session := byPane[entry.Pane]
		if session == nil {
			session = &SessionActivity{Session: entry.Label, Project: project(entry.Dir)}
			byPane[entry.Pane] = session
			order = append(order, session)
		}
		if entry.Label != "" {
			session.Session = entry.Label
		}
		session.at = entry.At
		session.State = map[string]string{"asked": "working", "step": "working", "answered": "idle"}[entry.Kind]
		if session.State == "" {
			session.State = entry.Kind
		}
		session.Entries = append(session.Entries, ActivityEntry{When: ago(now.Sub(time.UnixMilli(entry.At))), Kind: entry.Kind, Text: entry.Text})
	}
	var wait sync.WaitGroup
	for _, pane := range panes {
		session := byPane[pane.ID]
		if session != nil {
			session.Open, session.PaneID, session.Session = true, pane.ID, pane.Title()
			continue
		}
		session = &SessionActivity{Session: pane.Title(), PaneID: pane.ID, Project: project(pane.Dir), Open: true, State: "starting"}
		order = append(order, session)
		if !pane.Bridge {
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			glanceCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			raw, err := box.Transcript(glanceCtx, pane.ID, 12)
			if err != nil {
				return
			}
			glance := GlanceAt(ParseTranscript(raw), diaryAnswerSize)
			session.State, session.LastAnswer, session.at = glance.State, glance.Answer, glance.At
			if glance.Doing != "" {
				session.Entries = []ActivityEntry{{When: ago(now.Sub(time.UnixMilli(glance.At))), Kind: "step", Text: glance.Doing}}
			}
		}()
	}
	wait.Wait()
	sort.SliceStable(order, func(i, j int) bool { return order[i].at > order[j].at })
	listed := make([]SessionActivity, 0, len(order))
	for _, session := range order {
		if !session.Open {
			session.State = "closed after " + session.State
		}
		if session.at > 0 {
			session.LastActivity = ago(now.Sub(time.UnixMilli(session.at)))
		}
		if len(session.Entries) > entriesPerPane {
			session.Entries = session.Entries[len(session.Entries)-entriesPerPane:]
		}
		listed = append(listed, *session)
	}
	return listed, nil
}

// readDiary returns the entries since the cutoff from the end of the diary, oldest first.
func readDiary(path string, since int64) []diaryEntry {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	offset := max(info.Size()-diaryTail, 0)
	data, err := io.ReadAll(io.NewSectionReader(file, offset, info.Size()-offset))
	if err != nil {
		return nil
	}
	if offset > 0 {
		if cut := bytes.IndexByte(data, '\n'); cut >= 0 {
			data = data[cut+1:]
		}
	}
	var entries []diaryEntry
	for _, line := range bytes.Split(data, []byte("\n")) {
		var entry diaryEntry
		if json.Unmarshal(line, &entry) == nil && entry.Pane != "" && entry.At >= since {
			entries = append(entries, entry)
		}
	}
	return entries
}

func project(dir string) string {
	if _, rest, found := strings.Cut(dir, "/git/"); found {
		return rest
	}
	return dir
}
