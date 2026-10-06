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
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestJudge(t *testing.T) {
	sentence := Sentence{EN: "I don't want to cancel it now", ES: "No quiero cancelarlo ahora"}
	cases := map[string]string{
		"No quiero cancelarlo ahora.":       verdictRight,
		"uh, no quiero cancelar lo, ahora":  verdictRight,
		"no quiero cancelarlo aora":         verdictRight,
		"No quiero…":                        verdictPartial,
		"uhmm... maybe..":                   verdictPartial,
		"no quiero cancelarlo... uhh":       verdictPartial,
		"no quiero cancelar... ahora??":     verdictAttempt,
		"yo no quiero lo cancelar en ahora": verdictAttempt,
		"I don't know":                      verdictUnsure,
		"hmm, no idea":                      verdictUnsure,
		"no sé":                             verdictUnsure,
	}
	for said, want := range cases {
		if got := judge(said, sentence); got != want {
			t.Errorf("judge(%q) = %s, want %s", said, got, want)
		}
	}
	if judge("Yo no quiero", Sentence{ES: "Yo no quiero"}) != verdictRight || judge("No yo quiero", Sentence{ES: "Yo no quiero"}) != verdictAttempt {
		t.Error("word order slip not caught")
	}
}

func TestCued(t *testing.T) {
	turn := words("Nice. Now, how would you say it's not normal?")
	if cued(turn, "it is not normal") < 0 || cued(turn, "It's not normal") < 0 {
		t.Error("cue not found")
	}
	if cued(words("normal is normal in Spanish"), "it's not normal") >= 0 {
		t.Error("cue found in unrelated words")
	}
	if !backchannel(words("mm-hm")) || backchannel(words("so, how would you")) {
		t.Error("backchannel")
	}
}

func testLesson() Lesson {
	return Lesson{ID: "01", Title: "Test", Mission: "m", Close: "Tell me something true.", Thoughts: []Thought{
		{ID: "es", Title: "es", Sentences: []Sentence{{EN: "it's normal", ES: "Es normal"}, {EN: "it's not normal", ES: "No es normal"}}},
		{ID: "quiero", Title: "quiero", Sentences: []Sentence{{EN: "I want to cancel it", ES: "Quiero cancelarlo"}}},
	}}
}

func TestClassRefereesTheFloor(t *testing.T) {
	dir := t.TempDir()
	learner := OpenLearner(filepath.Join(dir, "learner.json"))
	logPath := filepath.Join(dir, "floor.jsonl")
	class := newClass(testLesson(), learner, logPath, time.Now())
	class.open()
	out := func(text string, ms int64) { class.watch("session.output_transcript.delta", text, ms, ms+100) }
	in := func(text string, ms int64) { class.watch("session.input_transcript.delta", text, ms, ms+100) }

	out("How would you say it's normal?", 1000)
	in("es normal", 4000)
	out("Good. And how would you say it's not normal?", 5000)
	in("no es", 9000)
	out("Mm", 9500)
	out("hm. Think about where", 10000)
	if class.verdict != verdictPartial || !class.held {
		t.Fatalf("verdict=%s held=%v", class.verdict, class.held)
	}
	time.Sleep(settleDelay + 200*time.Millisecond)
	select {
	case u := <-drain(class, "session.instructions.append"):
		if u.content != holdFloor {
			t.Fatalf("instruction %q", u.content)
		}
	default:
		t.Fatal("no intervention")
	}
	in("... normal", 30000)
	if class.outcomes["es/1"] != "alone" || class.outcomes["es/2"] != "alone" {
		t.Fatalf("outcomes %v", class.outcomes)
	}
	if !class.closing && !class.next {
		t.Fatal("next thought not handed over with the last prompt")
	}
	out("How would you say I want to cancel it?", 31000)
	in("Quiero lo cancelar?", 35000)
	out("Close. What does lo mean?", 36000)
	in("quiero cancelarlo", 40000)
	if class.outcomes["quiero/1"] != "helped" || !class.closing {
		t.Fatalf("outcomes %v closing=%v", class.outcomes, class.closing)
	}
	report := class.Finish("ls_1", time.Now())
	if !strings.Contains(report, "interventions: 1") || learner.Status("es") != "found alone" || learner.Status("quiero") != "found with help" {
		t.Fatalf("report=%s statuses=%s %s", report, learner.Status("es"), learner.Status("quiero"))
	}
	data, _ := os.ReadFile(logPath)
	for _, kind := range []string{`"kind":"prompt"`, `"kind":"attempt"`, `"kind":"verdict"`, `"kind":"intervene"`, `"kind":"resolved"`, `"kind":"close"`, `"kind":"closed"`} {
		if !strings.Contains(string(data), kind) {
			t.Errorf("floor log lacks %s", kind)
		}
	}
}

func TestClassLetsRosaAnswerAWrongAttempt(t *testing.T) {
	class := newClass(testLesson(), OpenLearner(""), "", time.Now())
	class.watch("session.output_transcript.delta", "How would you say it's not normal?", 1000, 1100)
	class.watch("session.input_transcript.delta", "es no normal", 3000, 3500)
	class.watch("session.output_transcript.delta", "Hm, where does the no go? No es normal.", 4000, 4100)
	time.Sleep(settleDelay + 100*time.Millisecond)
	if class.stops != 0 || class.outcomes["es/2"] != "shown" {
		t.Fatalf("stops=%d outcomes=%v", class.stops, class.outcomes)
	}
}

// drain returns the first queued update of kind, skipping the rest.
func drain(c *Class, kind string) chan update {
	found := make(chan update, 1)
	for {
		select {
		case u := <-c.outbox:
			if u.kind == kind {
				found <- u
				return found
			}
		default:
			return found
		}
	}
}

func TestManagerStartsRosa(t *testing.T) {
	attached := make(chan *websocket.Conn, 1)
	var created map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/live/sessions", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&created)
		fmt.Fprint(w, `{"session":{"id":"ls_rosa"},"transport":{"type":"webrtc","sdp":"v=0 answer"}}`)
	})
	mux.HandleFunc("/v1/live/sessions/ls_rosa/attach", func(w http.ResponseWriter, r *http.Request) {
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
	rosa := filepath.Join(dir, "rosa")
	os.MkdirAll(filepath.Join(rosa, "course"), 0o700)
	for name, text := range map[string]string{"identity.md": "rosa identity", "live.md": "rosa live", "backend.md": "rosa backend", "voice": "bossa\n"} {
		os.WriteFile(filepath.Join(rosa, name), []byte(text), 0o600)
	}
	lesson, _ := json.Marshal(testLesson())
	os.WriteFile(filepath.Join(rosa, "course", "lesson-01.json"), lesson, 0o600)
	m := NewManager(&fakeBox{transcripts: map[string][]json.RawMessage{}}, "key", func() (string, error) { return "carla persona", nil }, dir, filepath.Join(dir, "run", "memory.jsonl"))
	m.API = server.URL
	m.client = server.Client()
	m.Emit(Update{true, "Quill finished."})
	if _, err := m.Handle(context.Background(), `{"op":"start","agent":"rosa","voice":"quartz","accent":"Irish","sdp":"v=0 offer"}`); err != nil {
		t.Fatal(err)
	}
	session := created["session"].(map[string]any)
	instructions := session["instructions"].(string)
	if instructions != "rosa identity\n\nrosa live" || session["audio"].(map[string]any)["output"].(map[string]any)["voice"] != "bossa" {
		t.Fatalf("session=%v", session)
	}
	conn := <-attached
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(data), "session.thinking.append") || !strings.Contains(string(data), "Today's lesson") {
		t.Fatalf("lesson not handed over: %s %v", data, err)
	}
	m.Emit(Update{true, "Lara wrote in #dev."})
	m.mu.Lock()
	pending := len(m.pending)
	m.mu.Unlock()
	if pending != 2 {
		t.Fatalf("Carla's updates reached Rosa's call: pending=%d", pending)
	}
	if _, err := m.Handle(context.Background(), `{"op":"start","agent":"bob","voice":"quartz","sdp":"v=0 offer"}`); err == nil {
		t.Fatal("unknown agent accepted")
	}
	m.Handle(context.Background(), `{"op":"close"}`)
}
