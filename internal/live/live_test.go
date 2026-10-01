package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func entry(role string, at int64, blocks ...map[string]any) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"role": role, "at": at, "blocks": blocks})
	return data
}

func text(t string) map[string]any     { return map[string]any{"type": "text", "text": t} }
func thinking(t string) map[string]any { return map[string]any{"type": "thinking", "text": t} }
func call(id, name string, args map[string]any) map[string]any {
	return map[string]any{"type": "toolCall", "id": id, "name": name, "args": args}
}
func result(id, name, t string, failed bool) map[string]any {
	return map[string]any{"type": "toolResult", "id": id, "name": name, "text": t, "error": failed}
}

func TestParseTranscript(t *testing.T) {
	turns := ParseTranscript([]json.RawMessage{
		entry("user", 1, text("pick a ticket")),
		entry("assistant", 2, thinking("plan"), call("c1", "read", map[string]any{"path": "/x/skills/review/SKILL.md"}), call("c2", "bash", map[string]any{"command": "ls"})),
		entry("toolResult", 3, result("c2", "bash", "a b", false)),
		entry("assistant", 4, text("Done.")),
	})
	kinds := []string{}
	for _, turn := range turns {
		kinds = append(kinds, turn.Kind)
	}
	if strings.Join(kinds, ",") != "lemon,carla,skill,tool,carla" {
		t.Fatalf("kinds=%v", kinds)
	}
	if turns[2].Name != "review" || turns[3].Result != "a b" || turns[3].At != 3 || turns[3].Summary != "ls" {
		t.Fatalf("turns=%+v", turns)
	}
}

func TestNarratorWalkthrough(t *testing.T) {
	ask := Turn{Kind: "lemon", At: 1, Text: "pick a random ticket of Lara's"}
	n := NewNarrator("Quill", ask.Text, Narrate)
	base := time.Unix(1000, 0)
	if updates := n.Digest([]Turn{{Kind: "lemon", Text: "earlier"}}, base); len(updates) != 0 {
		t.Fatalf("spoke before the ask: %v", updates)
	}
	plan := Turn{Kind: "carla", At: 2, Thinking: "Need Lara's tickets.\n\nI'll search Linear by assignee."}
	pending := Turn{Kind: "tool", At: 3, Name: "linear_search_issues", Summary: "assignee:lara"}
	updates := n.Digest([]Turn{ask, plan, pending}, base)
	want := []Update{
		{true, "Quill has started on: pick a random ticket of Lara's. Progress comes as it happens."},
		{true, "Quill's plan: I'll search Linear by assignee."},
	}
	if fmt.Sprint(updates) != fmt.Sprint(want) {
		t.Fatalf("updates=%v", updates)
	}
	ran := pending
	ran.At, ran.Result = 4, "12 issues"
	more := Turn{Kind: "tool", At: 5, Name: "read", Summary: "/tmp/a", Result: "ok"}
	if updates := n.Digest([]Turn{ask, plan, ran, more}, base.Add(10*time.Second)); len(updates) != 0 {
		t.Fatalf("spoke tools too early: %v", updates)
	}
	failed := Turn{Kind: "tool", At: 6, Name: "read", Summary: "/tmp/x", Result: "no such file", Error: true}
	updates = n.Digest([]Turn{ask, plan, ran, more, failed}, base.Add(15*time.Second))
	if fmt.Sprint(updates) != fmt.Sprint([]Update{{true, "0 min in, read failed in Quill: no such file"}}) {
		t.Fatalf("updates=%v", updates)
	}
	again := Turn{Kind: "tool", At: 7, Name: "bash", Summary: "git status", Result: "clean"}
	updates = n.Digest([]Turn{ask, plan, ran, more, failed, again}, base.Add(60*time.Second+quietPeriod))
	if fmt.Sprint(updates) != fmt.Sprint([]Update{{true, "1 min in, Quill is still going: 3 steps since last time (linear_search_issues, read, bash), now on bash git status"}}) {
		t.Fatalf("updates=%v", updates)
	}
	answer := Turn{Kind: "carla", At: 8, Text: "JRN-12: fix the thing."}
	all := []Turn{ask, plan, ran, more, failed, again, answer}
	updates = n.Digest(all, base.Add(90*time.Second))
	if fmt.Sprint(updates) != fmt.Sprint([]Update{{true, "Quill says: JRN-12: fix the thing."}}) || n.Done {
		t.Fatalf("updates=%v done=%v", updates, n.Done)
	}
	updates = n.Digest(all, base.Add(93*time.Second))
	if fmt.Sprint(updates) != fmt.Sprint([]Update{{false, "Quill is idle now; that was its final answer."}}) || !n.Done {
		t.Fatalf("updates=%v done=%v", updates, n.Done)
	}
	if updates := n.Digest(all, base.Add(100*time.Second)); len(updates) != 0 {
		t.Fatalf("spoke after done: %v", updates)
	}
}

func TestNarratorBackground(t *testing.T) {
	n := NewNarrator("Quill", "do it", Background)
	base := time.Unix(1000, 0)
	steps := []Turn{{Kind: "lemon", At: 1, Text: "do it"}, {Kind: "carla", At: 2, Thinking: "hmm"}, {Kind: "tool", At: 3, Name: "bash", Summary: "ls", Result: "ok"}}
	if updates := n.Digest(steps, base); fmt.Sprint(updates) != fmt.Sprint([]Update{{false, "Quill is working on this in the background: do it"}}) {
		t.Fatalf("updates=%v", updates)
	}
	done := append(steps, Turn{Kind: "carla", At: 4, Text: "All done."})
	if updates := n.Digest(done, base.Add(time.Second)); len(updates) != 0 {
		t.Fatalf("spoke unstable answer: %v", updates)
	}
	if updates := n.Digest(done, base.Add(4*time.Second)); fmt.Sprint(updates) != fmt.Sprint([]Update{{true, "Quill finished the background task. All done."}}) || !n.Done {
		t.Fatalf("updates=%v", updates)
	}
}

func TestMemoryLogAndSeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	m := OpenMemory(path)
	base := time.Unix(1000, 0)
	m.Add("user", "pick a ", base)
	m.Add("user", "ticket", base.Add(500*time.Millisecond))
	m.Add("assistant", "on it", base.Add(2*time.Second))
	m.Add("user", "thanks", base.Add(4*time.Second))
	if err := m.Flush(false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Count(string(data), "\n") != 2 {
		t.Fatalf("log=%q", data)
	}
	if err := m.Flush(true); err != nil {
		t.Fatal(err)
	}
	seed := OpenMemory(path).Seed(base.Add(65 * time.Second))
	roles := []string{}
	for _, item := range seed {
		roles = append(roles, item["role"].(string))
	}
	if strings.Join(roles, ",") != "developer,user,assistant,user" {
		t.Fatalf("roles=%v", roles)
	}
	note := seed[0]["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(note, "1 minutes ago") || !strings.Contains(note, "carry straight on") || strings.Contains(note, "out of date") {
		t.Fatalf("note=%q", note)
	}
	note = OpenMemory(path).Seed(base.Add(3 * time.Hour))[0]["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(note, "2 hours ago") || !strings.Contains(note, "out of date") || !strings.Contains(note, "Delegate a check of the live state") || strings.Contains(note, "carry straight on") {
		t.Fatalf("note=%q", note)
	}
	if got := seed[1]["content"].([]map[string]any)[0]["text"]; got != "pick a ticket" {
		t.Fatalf("merged=%q", got)
	}
	old := OpenMemory(path).Seed(base.Add(memoryWindow + time.Hour))
	if len(old) != 1 {
		t.Fatalf("seeded stale history: %v", old)
	}
	if note := old[0]["content"].([]map[string]any)[0]["text"].(string); !strings.Contains(note, "3 hours ago") || !strings.Contains(note, "out of date") || strings.Contains(note, "follows") {
		t.Fatalf("note=%q", note)
	}
	empty := OpenMemory("").Seed(base)
	if note := empty[0]["content"].([]map[string]any)[0]["text"].(string); len(empty) != 1 || !strings.Contains(note, "no earlier voice conversation") || !strings.Contains(note, "out of date") {
		t.Fatalf("empty=%v", empty)
	}
}

// fakeBox is a box with one project and panes that appear on spawn.
type fakeBox struct {
	mu          sync.Mutex
	panes       []Pane
	spawned     []string
	sent        []string
	transcripts map[string][]json.RawMessage
}

func (b *fakeBox) Panes(context.Context) ([]Pane, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Pane(nil), b.panes...), nil
}
func (b *fakeBox) Projects() ([]Project, error) {
	return []Project{{Name: "quill", Path: "/home/lemon/git/ee/quill"}}, nil
}
func (b *fakeBox) Spawn(_ context.Context, dir, label string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spawned = append(b.spawned, label)
	b.panes = append(b.panes, Pane{ID: fmt.Sprintf("%%%d", 10+len(b.panes)), Label: label, Dir: dir, Command: "pi", Bridge: true})
	return nil
}
func (b *fakeBox) Kill(context.Context, string) error { return nil }
func (b *fakeBox) Send(_ context.Context, paneID, prompt string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, paneID+":"+prompt)
	b.transcripts[paneID] = []json.RawMessage{entry("user", 1, text(prompt)), entry("assistant", 2, text("Done: "+prompt))}
	return nil
}
func (b *fakeBox) Transcript(_ context.Context, paneID string, _ int) ([]json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.transcripts[paneID], nil
}
func (b *fakeBox) Screen(context.Context, string, int) (string, error) { return "screen", nil }

func TestDispatcherQueuesAndNarrates(t *testing.T) {
	box := &fakeBox{transcripts: map[string][]json.RawMessage{}}
	var mu sync.Mutex
	var updates []Update
	d := NewDispatcher(box, NewDiscord(func(Update) {}), func(u Update) { mu.Lock(); updates = append(updates, u); mu.Unlock() })
	started := time.Now()
	result, err := d.Call(context.Background(), "ask_project", `{"project_path":"/home/lemon/git/ee/quill","text":"status?","mode":"narrate"}`)
	if err != nil || time.Since(started) > 2*time.Second {
		t.Fatalf("ask_project err=%v took=%v", err, time.Since(started))
	}
	if !strings.Contains(fmt.Sprint(result), "queued") {
		t.Fatalf("result=%v", result)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(updates)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(box.spawned) != 1 || box.spawned[0] != "" || len(box.sent) != 1 {
		t.Fatalf("spawned=%v sent=%v", box.spawned, box.sent)
	}
	joined := fmt.Sprint(updates)
	if !strings.Contains(joined, "has started on: status?") || !strings.Contains(joined, "says: Done: status?") || !strings.Contains(joined, "idle now") {
		t.Fatalf("updates=%v", updates)
	}
	tickets := d.Tickets()
	if len(tickets) != 1 || tickets[0].State != "done" {
		t.Fatalf("tickets=%+v", tickets)
	}
	if _, err := d.Call(context.Background(), "ask_pane", `{"pane_id":"%99","text":"x","mode":"narrate"}`); err == nil {
		t.Fatal("asked an unknown pane")
	}
}

func TestBackendToolLoop(t *testing.T) {
	var calls []string
	rounds := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []json.RawMessage `json:"input"`
			Store bool              `json:"store"`
			Text  struct {
				Format struct {
					Type string `json:"type"`
				} `json:"format"`
			} `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Text.Format.Type != "json_schema" {
			t.Errorf("format=%q", body.Text.Format.Type)
		}
		rounds++
		if rounds == 1 {
			fmt.Fprint(w, `{"output":[{"type":"reasoning","id":"rs_1","summary":[]},{"type":"function_call","call_id":"c1","name":"tickets","arguments":"{}"}]}`)
			return
		}
		if len(body.Input) < 4 || !strings.Contains(string(body.Input[len(body.Input)-1]), "function_call_output") {
			t.Errorf("continuation input=%s", body.Input)
		}
		fmt.Fprint(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"Nothing running.\",\"details\":\"No tickets.\"}"}]}]}`)
	}))
	defer server.Close()
	b := &Backend{Client: server.Client(), URL: server.URL, Key: "k", Model: "m", Tools: func(_ context.Context, name, _ string) (any, error) {
		calls = append(calls, name)
		return map[string]any{"tickets": []Ticket{}}, nil
	}}
	reply, err := b.Answer(context.Background(), []map[string]any{seedMessage("user", "anything running?")})
	if err != nil || reply.Say != "Nothing running." || reply.Details != "No tickets." || fmt.Sprint(calls) != "[tickets]" {
		t.Fatalf("reply=%+v err=%v calls=%v", reply, err, calls)
	}
	if len(reply.Fetched) != 1 || reply.Fetched[0].Tool != "tickets" || reply.Fetched[0].Result != `{"tickets":[]}` {
		t.Fatalf("fetched=%+v", reply.Fetched)
	}
}

// fakeConn feeds scripted events and records what the session writes.
type fakeConn struct {
	events chan []byte
	mu     sync.Mutex
	wrote  []map[string]any
}

func (c *fakeConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case data, ok := <-c.events:
		if !ok {
			return nil, fmt.Errorf("closed")
		}
		return data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *fakeConn) Write(_ context.Context, event []byte) error {
	var parsed map[string]any
	json.Unmarshal(event, &parsed)
	c.mu.Lock()
	c.wrote = append(c.wrote, parsed)
	c.mu.Unlock()
	return nil
}
func (c *fakeConn) Close() error { return nil }

func TestSessionDelegatesAndRemembers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		texts := []string{}
		for _, item := range body.Input {
			texts = append(texts, item.Role+": "+item.Content[0].Text)
		}
		if len(texts) != 5 || !strings.Contains(texts[0], "You and Lemon last spoke 30 minutes ago.") || !strings.Contains(texts[0], "Check the live state") || !strings.HasSuffix(texts[0], "Dispatcher state.") ||
			texts[1] != "user: (30 minutes ago) how is quill" || texts[2] != "developer: (This call starts here.)" || texts[3] != "user: ask quill for status" {
			t.Errorf("input=%q", texts)
		}
		fmt.Fprint(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"Quill is queued.\",\"details\":\"\"}"}]}]}`)
	}))
	defer server.Close()
	conn := &fakeConn{events: make(chan []byte, 8)}
	memory := OpenMemory("")
	memory.Add("user", "how is quill", time.Now().Add(-30*time.Minute))
	backend := &Backend{Client: server.Client(), URL: server.URL, Key: "k", Model: "m", Tools: func(context.Context, string, string) (any, error) { return nil, nil }}
	session := NewSession("ls_1", conn, memory, &Recall{}, backend, func() string { return "Dispatcher state." })
	go session.Run()
	conn.events <- []byte(`{"type":"session.input_transcript.delta","delta":"ask quill for status"}`)
	conn.events <- []byte(`{"type":"session.delegation.created","delegation":{"id":"item_1","target":"client"}}`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn.mu.Lock()
		n := len(conn.wrote)
		conn.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	conn.events <- []byte(`{"type":"session.closed","reason":"close_requested"}`)
	<-session.Closed
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.wrote) != 1 || conn.wrote[0]["type"] != "session.commentary.append" || conn.wrote[0]["delegation_id"] != "item_1" || conn.wrote[0]["content"] != "Quill is queued." {
		t.Fatalf("wrote=%v", conn.wrote)
	}
	if recent := memory.Recent(time.Now(), 5); len(recent) != 2 || recent[1].Text != "ask quill for status" {
		t.Fatalf("memory=%v", recent)
	}
}

func TestSessionRecallsFollowUps(t *testing.T) {
	var inputs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		last := string(body.Input[len(body.Input)-1])
		inputs = append(inputs, string(body.Input[0])+"\n"+string(body.Input[1]))
		switch {
		case len(inputs) == 1:
			fmt.Fprint(w, `{"output":[{"type":"function_call","call_id":"c1","name":"activity","arguments":"{}"}]}`)
		case strings.Contains(last, "function_call_output"):
			fmt.Fprint(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"Two sessions are moving.\",\"details\":\"Quill: working on the dark theme. Brood: idle.\"}"}]}]}`)
		default:
			fmt.Fprint(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"Quill's on the dark theme.\",\"details\":\"\"}"}]}]}`)
		}
	}))
	defer server.Close()
	conn := &fakeConn{events: make(chan []byte, 8)}
	memory := OpenMemory("")
	memory.Add("user", "morning", time.Now().Add(-30*time.Minute))
	recall := &Recall{}
	backend := &Backend{Client: server.Client(), URL: server.URL, Key: "k", Model: "m", Tools: func(context.Context, string, string) (any, error) {
		return map[string]any{"sessions": "Quill working, Brood idle"}, nil
	}}
	session := NewSession("ls_1", conn, memory, recall, backend, func() string { return "Dispatcher state." })
	go session.Run()
	wait := func(n int) {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			conn.mu.Lock()
			got := len(conn.wrote)
			conn.mu.Unlock()
			if got >= n {
				return
			}
		}
		t.Fatalf("waited for %d writes", n)
	}
	conn.events <- []byte(`{"type":"session.input_transcript.delta","delta":"how are things going"}`)
	conn.events <- []byte(`{"type":"session.delegation.created","delegation":{"id":"item_1","target":"client"}}`)
	wait(2)
	recall.Note("Brood says: shipped.", time.Now())
	conn.events <- []byte(`{"type":"session.input_transcript.delta","delta":"what is quill on"}`)
	conn.events <- []byte(`{"type":"session.delegation.created","delegation":{"id":"item_2","target":"client"}}`)
	wait(3)
	conn.events <- []byte(`{"type":"session.closed","reason":"close_requested"}`)
	<-session.Closed
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.wrote[0]["type"] != "session.thinking.append" || !strings.Contains(conn.wrote[0]["content"].(string), "Quill: working on the dark theme.") ||
		conn.wrote[1]["type"] != "session.commentary.append" || conn.wrote[1]["content"] != "Two sessions are moving." || conn.wrote[1]["delegation_id"] != "item_1" ||
		conn.wrote[2]["content"] != "Quill's on the dark theme." || conn.wrote[2]["delegation_id"] != "item_2" {
		t.Fatalf("wrote=%v", conn.wrote)
	}
	if len(inputs) != 3 || !strings.Contains(inputs[0], "Check the live state") {
		t.Fatalf("first input=%q", inputs)
	}
	followUp := inputs[2]
	for _, want := range []string{"Recent results, oldest first.", "Lemon said: how are things going", `activity returned: {\"sessions\":\"Quill working, Brood idle\"}`, "You answered: Two sessions are moving.", "Details: Quill: working on the dark theme.", "Updates since:", "Brood says: shipped."} {
		if !strings.Contains(followUp, want) {
			t.Errorf("follow-up input lacks %q: %s", want, followUp)
		}
	}
	if strings.Contains(followUp, "Check the live state") {
		t.Errorf("follow-up still told to check live state: %s", followUp)
	}
}

func TestRecallKeepsRecentExchanges(t *testing.T) {
	recall := &Recall{}
	now := time.Now()
	if recall.Brief(now) != "" || recall.Checked(now.Add(-time.Hour)) {
		t.Fatal("empty recall reported results")
	}
	recall.Note("before any exchange", now.Add(-10*time.Minute))
	for i := 0; i < 7; i++ {
		recall.Add(Exchange{At: now.Add(time.Duration(i-7) * time.Minute), Asked: fmt.Sprintf("question %d", i), Fetched: []Fetched{{"list_panes", "{}", strings.Repeat("x", 3000)}}, Said: fmt.Sprintf("answer %d", i)})
	}
	brief := recall.Brief(now)
	if strings.Contains(brief, "question 0") || strings.Contains(brief, "question 1") || !strings.Contains(brief, "5 minutes ago, Lemon said: question 2") || !strings.Contains(brief, "question 6") {
		t.Fatalf("kept the wrong exchanges: %s", brief)
	}
	if strings.Index(brief, "question 2") > strings.Index(brief, "question 6") || strings.Contains(brief, strings.Repeat("x", recallResult+1)) || strings.Contains(brief, "before any exchange") {
		t.Fatalf("order, truncation or old updates wrong: %s", brief)
	}
	if !recall.Checked(now.Add(-2*time.Minute)) || recall.Checked(now) {
		t.Fatal("Checked ignores exchange times")
	}
}

func TestManagerStartAttachesSideband(t *testing.T) {
	attached := make(chan *websocket.Conn, 1)
	var created map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/live/sessions", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&created)
		fmt.Fprint(w, `{"session":{"id":"ls_test"},"transport":{"type":"webrtc","sdp":"v=0 answer"}}`)
	})
	mux.HandleFunc("/v1/live/sessions/ls_test/attach", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("auth=%q", r.Header.Get("Authorization"))
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		attached <- conn
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "live.md"), []byte("live rules"), 0o600)
	os.WriteFile(filepath.Join(dir, "backend.md"), []byte("backend rules"), 0o600)
	m := NewManager(&fakeBox{transcripts: map[string][]json.RawMessage{}}, "key", func() (string, error) { return "persona", nil }, dir, filepath.Join(dir, "memory.jsonl"))
	m.API = server.URL
	m.client = server.Client()
	m.Emit(Update{true, "Quill finished while you were away."})
	reply, err := m.Handle(context.Background(), `{"op":"start","voice":"quartz","accent":"New Zealand","sdp":"v=0 offer"}`)
	if err != nil || !strings.Contains(reply, "v=0 answer") {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	session := created["session"].(map[string]any)
	if session["delegation"].(map[string]any)["type"] != "client" || !strings.Contains(session["instructions"].(string), "New Zealand") || !strings.HasPrefix(session["instructions"].(string), "persona\n\nlive rules") {
		t.Fatalf("session=%v", session)
	}
	conn := <-attached
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(data), "session.commentary.append") || !strings.Contains(string(data), ", while the call was off) Quill finished while you were away.") {
		t.Fatalf("pending update not flushed: %s %v", data, err)
	}
	status, _ := m.Handle(context.Background(), `{"op":"status"}`)
	if !strings.Contains(status, `"session":"ls_test"`) || !strings.Contains(status, `"pending":0`) {
		t.Fatalf("status=%s", status)
	}
	if _, err := m.Handle(context.Background(), `{"op":"said","text":"hey, any news?"}`); err != nil {
		t.Fatal(err)
	}
	_, data, _ = conn.Read(ctx)
	if !strings.Contains(string(data), "session.instructions.append") || !strings.Contains(string(data), "hey, any news?") {
		t.Fatalf("said not relayed: %s", data)
	}
	m.Handle(context.Background(), `{"op":"discord","messages":[{"phone_id":4,"from":"Lara","in":"#dev","text":"lunch?","at":1,"can_reply":true}]}`)
	_, data, _ = conn.Read(ctx)
	if !strings.Contains(string(data), "Discord message 1 from Lara in #dev: lunch?") {
		t.Fatalf("discord not announced: %s", data)
	}
	if result, err := m.dispatcher.Call(context.Background(), "discord_reply", `{"id":1,"text":"yes"}`); err != nil || !strings.Contains(fmt.Sprint(result), "on its way") {
		t.Fatalf("reply=%v err=%v", result, err)
	}
	status, _ = m.Handle(context.Background(), `{"op":"status"}`)
	if !strings.Contains(status, `"phone_id":4`) || !strings.Contains(status, `"text":"yes"`) {
		t.Fatalf("status=%s", status)
	}
	m.Handle(context.Background(), `{"op":"discord-done","id":1,"error":""}`)
	status, _ = m.Handle(context.Background(), `{"op":"status"}`)
	if strings.Contains(status, `"text":"yes"`) {
		t.Fatalf("reply still pending: %s", status)
	}
	m.Handle(context.Background(), `{"op":"close"}`)
}

func TestConversationTool(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(&fakeBox{transcripts: map[string][]json.RawMessage{}}, "key", func() (string, error) { return "persona", nil }, dir, filepath.Join(dir, "memory.jsonl"))
	if _, err := m.call(context.Background(), "conversation", `{"state":"later"}`); err == nil {
		t.Fatal("bad state accepted")
	}
	if _, err := m.call(context.Background(), "conversation", `{"state":"doze"}`); err == nil {
		t.Fatal("doze accepted")
	}
	result, err := m.call(context.Background(), "conversation", `{"state":"off"}`)
	if err != nil || !strings.Contains(fmt.Sprint(result), "off") {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if got := m.Status().Phone; got != "off" {
		t.Fatalf("phone=%q", got)
	}
	if got := m.Status().Phone; got != "" {
		t.Fatalf("phone not consumed: %q", got)
	}
}

func TestMemorySeedMarksPauses(t *testing.T) {
	m := OpenMemory("")
	base := time.Unix(1000, 0)
	m.Add("user", "first", base)
	m.Add("assistant", "yep", base.Add(2*time.Second))
	m.Add("user", "later", base.Add(25*time.Minute))
	seed := m.Seed(base.Add(26 * time.Minute))
	if len(seed) != 5 {
		t.Fatalf("seed=%d", len(seed))
	}
	pause := seed[3]["content"].([]map[string]any)[0]["text"].(string)
	if pause != "(24 minutes pass in silence)" && pause != "(25 minutes pass in silence)" {
		t.Fatalf("pause=%q", pause)
	}
}

func TestGlanceAt(t *testing.T) {
	if got := GlanceAt(nil, 10); got.State != "idle" {
		t.Fatalf("empty=%+v", got)
	}
	turns := []Turn{{Kind: "lemon", At: 1, Text: "go"}, {Kind: "carla", At: 2, Text: "first answer"}, {Kind: "tool", At: 3, Name: "bash", Summary: "ls"}}
	if got := GlanceAt(turns, 5); got.State != "working" || got.Doing != "bash ls" || got.Answer != "first" || got.At != 3 {
		t.Fatalf("working=%+v", got)
	}
	if got := GlanceAt(append(turns, Turn{Kind: "carla", At: 4, Text: "done"}), 50); got.State != "idle" || got.Doing != "" || got.Answer != "done" {
		t.Fatalf("idle=%+v", got)
	}
}

func TestActivityReadsDiaryAndFallsBack(t *testing.T) {
	now := time.UnixMilli(10_000_000)
	path := filepath.Join(t.TempDir(), "activity.jsonl")
	lines := []string{
		`{"at":1000,"pane":"%1","label":"Old","cwd":"/home/lemon/git/a/b","kind":"answered","text":"too old"}`,
		`not json`,
		`{"at":9000000,"pane":"%1","label":"Grant Coverage","cwd":"/home/lemon/git/hm/brood","kind":"asked","text":"cover UK funds"}`,
		`{"at":9500000,"pane":"%1","label":"Grant Coverage","cwd":"/home/lemon/git/hm/brood","kind":"step","text":"Norway done, starting UK"}`,
		`{"at":9900000,"pane":"%2","label":"Gone","cwd":"/home/lemon/git/x/y","kind":"answered","text":"shipped"}`,
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	box := &fakeBox{panes: []Pane{{ID: "%1", Label: "Grant Coverage", Bridge: true}, {ID: "%3", Label: "Quiet", Dir: "/home/lemon/git/q/r", Bridge: true}},
		transcripts: map[string][]json.RawMessage{"%3": {json.RawMessage(`{"role":"assistant","at":5000000,"blocks":[{"type":"text","text":"all done"}]}`)}}}
	sessions, err := Activity(context.Background(), box, path, now, time.Hour)
	if err != nil || len(sessions) != 3 {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	gone, grant, quiet := sessions[0], sessions[1], sessions[2]
	if gone.Session != "Gone" || gone.Open || gone.State != "closed after idle" {
		t.Fatalf("gone=%+v", gone)
	}
	if grant.State != "working" || !grant.Open || grant.Project != "hm/brood" || len(grant.Entries) != 2 || grant.Entries[1].Text != "Norway done, starting UK" {
		t.Fatalf("grant=%+v", grant)
	}
	if quiet.State != "idle" || quiet.LastAnswer != "all done" || quiet.Project != "q/r" {
		t.Fatalf("quiet=%+v", quiet)
	}
}
