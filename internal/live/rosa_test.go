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
		"no kiero cancellarlo ahora":        verdictRight,
		"no quiero cancelar ahora":          verdictAttempt,
		"no quiero cancelarlos ahora":       verdictAttempt,
		"No quiero…":                        verdictPartial,
		"uhmm... maybe..":                   verdictPartial,
		"no quiero cancelarlo... uhh":       verdictPartial,
		"no quiero cancelar... ahora??":     verdictAttempt,
		"yo no quiero lo cancelar en ahora": verdictAttempt,
		"I don't know":                      verdictUnsure,
		"hmm, no idea":                      verdictUnsure,
		"no sé":                             verdictUnsure,
		"no quiero cancelarlo right now":    verdictGap,
		"no quiero... about":                verdictPartial,
	}
	for said, want := range cases {
		if got := judge(said, sentence); got != want {
			t.Errorf("judge(%q) = %s, want %s", said, got, want)
		}
	}
	if judge("mi vida es diferentes", Sentence{ES: "mi vida es diferente"}) != verdictAttempt || judge("es no normal", Sentence{ES: "no es normal"}) != verdictAttempt {
		t.Error("a real miss judged right")
	}
	if judge("Naturalmennte", Sentence{ES: "naturalmente"}) != verdictRight || judge("es increible", Sentence{ES: "es increíble"}) != verdictRight {
		t.Error("a transcription slip judged wrong")
	}
	if switched("why is el mundo at the end of the sentence when we say el mundo") || !switched("quiero hablar más rápido, yyy.. about many different topics") {
		t.Error("code switch detection")
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

func testGraph(t *testing.T, dir string) *Graph {
	thoughts := []Thought{
		{ID: "t02-es", Title: "es and no es", Kind: "structure"},
		{ID: "t04-quiero", Title: "quiero", Kind: "structure", Needs: []string{"t02-es"}},
		{ID: "t05-yo", Title: "yo for emphasis", Kind: "structure", Needs: []string{"t04-quiero"}},
	}
	data, _ := json.Marshal(thoughts)
	os.MkdirAll(filepath.Join(dir, "graph"), 0o700)
	os.WriteFile(filepath.Join(dir, "graph", "complete-spanish-01.json"), data, 0o600)
	graph, err := LoadGraph(filepath.Join(dir, "graph"), filepath.Join(dir, "thoughts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func testPlan() Plan {
	return Plan{Mode: "teach", Why: "first call", Items: []PlanItem{
		{Thought: "t02-es", Kind: "new", Why: "start", Targets: []Sentence{{EN: "it's normal", ES: "es normal"}, {EN: "it's not normal", ES: "no es normal"}}},
		{Thought: "t04-quiero", Kind: "new", Why: "next", Targets: []Sentence{{EN: "I want to cancel it", ES: "quiero cancelarlo"}}},
	}}
}

func TestClassRefereesTheFloor(t *testing.T) {
	dir := t.TempDir()
	learner := OpenLearner(filepath.Join(dir, "learner.json"))
	logPath := filepath.Join(dir, "floor.jsonl")
	class := newClass(testGraph(t, dir), learner, logPath, time.Now())
	prompted := make(chan bool, 1)
	class.prompted = func() { prompted <- true }
	exhausted := make(chan bool, 1)
	class.exhausted = func() { exhausted <- true }
	class.SetPlan(testPlan())
	out := func(text string, ms int64) { class.watch("session.output_transcript.delta", text, ms, ms+100) }
	in := func(text string, ms int64) { class.watch("session.input_transcript.delta", text, ms, ms+100) }

	out("How would you say it's normal?", 1000)
	in("es normal", 4000)
	select {
	case <-prompted:
	case <-time.After(time.Second):
		t.Fatal("the first prompt did not trigger the adjust pass")
	}
	out("Good. And now it's not normal, che, how would you say that?", 5000)
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
	if class.outcomes["1:t02-es/1"] != "alone" || class.outcomes["1:t02-es/2"] != "alone" || !class.handed[1] {
		t.Fatalf("outcomes %v handed %v", class.outcomes, class.handed)
	}
	out("How would you say I want to cancel it?", 31000)
	in("Quiero lo cancelar?", 35000)
	out("Close. What does lo mean?", 36000)
	in("quiero cancelarlo", 40000)
	if class.outcomes["2:t04-quiero/1"] != "helped" || !class.extending {
		t.Fatalf("outcomes %v extending=%v", class.outcomes, class.extending)
	}
	select {
	case <-exhausted:
	case <-time.After(time.Second):
		t.Fatal("the plan ran out without asking for more")
	}
	class.Extend([]PlanItem{{Thought: "t05-yo", Kind: "new", Why: "more", Targets: []Sentence{{EN: "I want it", ES: "yo lo quiero"}}}})
	if class.extending || !class.handed[2] {
		t.Fatal("extension not handed over")
	}
	report := class.Finish("ls_1", time.Now())
	if !strings.Contains(report, "interventions: 1") || learner.Status("t02-es") != "found alone" || learner.Status("t04-quiero") != "found with help" {
		t.Fatalf("report=%s statuses=%s %s", report, learner.Status("t02-es"), learner.Status("t04-quiero"))
	}
	if !learner.Known("quiero cancelarlo") || learner.Known("sobre") {
		t.Fatal("dictionary not fed by resolved targets")
	}
	data, _ := os.ReadFile(logPath)
	for _, kind := range []string{`"kind":"plan"`, `"kind":"prompt"`, `"kind":"attempt"`, `"kind":"verdict"`, `"kind":"intervene"`, `"kind":"resolved"`, `"kind":"exhausted"`, `"kind":"extended"`, `"kind":"closed"`} {
		if !strings.Contains(string(data), kind) {
			t.Errorf("floor log lacks %s", kind)
		}
	}
}

func TestClassRefereesAnImprovisedPrompt(t *testing.T) {
	class := newClass(testGraph(t, t.TempDir()), OpenLearner(""), "", time.Now())
	class.SetPlan(testPlan())
	class.watch("session.output_transcript.delta", "Okay, digital, like your software. How would you say it?", 1000, 1100)
	class.watch("session.input_transcript.delta", "di...", 3000, 3100)
	if class.armed == nil || class.key(class.armed) != "adhoc" || class.verdict != verdictPartial {
		t.Fatalf("armed %v verdict %s", class.armed, class.verdict)
	}
	class.watch("session.output_transcript.delta", "Come on, like your software.", 4000, 4100)
	time.Sleep(settleDelay + 100*time.Millisecond)
	class.mu.Lock()
	stops := class.stops
	class.mu.Unlock()
	if stops != 1 {
		t.Fatalf("stops=%d", stops)
	}
	class.watch("session.input_transcript.delta", "digital", 9000, 9100)
	class.watch("session.output_transcript.delta", "Perfecto. Before we go on, how do you say life?", 10000, 10100)
	class.watch("session.input_transcript.delta", "how do I say life", 12000, 12100)
	if class.verdict != verdictUnsure {
		t.Fatalf("a question to Rosa judged %s", class.verdict)
	}
}

func TestClassReleasesAHold(t *testing.T) {
	class := newClass(testGraph(t, t.TempDir()), OpenLearner(""), "", time.Now())
	class.SetPlan(testPlan())
	out := func(text string, ms int64) { class.watch("session.output_transcript.delta", text, ms, ms+100) }
	in := func(text string, ms int64) { class.watch("session.input_transcript.delta", text, ms, ms+100) }
	notes := func() string {
		text := ""
		for len(class.outbox) > 0 {
			text += (<-class.outbox).content + "\n"
		}
		return text
	}

	// A question about Spanish is not a translation prompt: an English answer is not partial.
	out("Let's start with what you own. In Norwegian you say normal. Where's the stress there?", 1000)
	in("On the R", 3000)
	out("Exactly, al final.", 3400)
	time.Sleep(settleDelay + 100*time.Millisecond)
	if class.stops != 0 {
		t.Fatalf("stopped Rosa answering a question: armed %v verdict %s", class.armed, class.verdict)
	}

	out("Now, how would you say it's not normal?", 5000)
	in("no es", 8000)
	out("Hm, and", 8400)
	time.Sleep(settleDelay + 100*time.Millisecond)
	notes()
	class.mu.Lock()
	holding := class.holding
	class.mu.Unlock()
	if !holding {
		t.Fatal("no hold")
	}
	in(" Sorry, you dropped off. Hello?", 12000)
	if text := notes(); !strings.Contains(text, answerHim) || class.holding {
		t.Fatalf("hold not released when he spoke to her: %s", text)
	}

	holdLimit = 300 * time.Millisecond
	defer func() { holdLimit = 8 * time.Second }()
	out("Okay. How would you say I want to cancel it?", 14000)
	in("quiero", 16000)
	out("Mm, and the", 16400)
	time.Sleep(settleDelay + holdLimit + 200*time.Millisecond)
	if text := notes(); !strings.Contains(text, holdFloor) || !strings.Contains(text, releaseFloor) {
		t.Fatalf("hold did not expire after silence: %s", text)
	}
}

func TestClassLetsRosaAnswerAWrongAttempt(t *testing.T) {
	class := newClass(testGraph(t, t.TempDir()), OpenLearner(""), "", time.Now())
	class.SetPlan(testPlan())
	class.watch("session.output_transcript.delta", "How would you say it's not normal?", 1000, 1100)
	class.watch("session.input_transcript.delta", "es no normal", 3000, 3500)
	class.watch("session.output_transcript.delta", "Hm, where does the no go? No es normal.", 4000, 4100)
	time.Sleep(settleDelay + 100*time.Millisecond)
	if class.stops != 0 || class.outcomes["1:t02-es/2"] != "shown" {
		t.Fatalf("stops=%d outcomes=%v", class.stops, class.outcomes)
	}
}

func TestClassWorksOutACodeSwitch(t *testing.T) {
	learner := OpenLearner("")
	learner.Update(func(l *Learner) { l.expose("es muy diferente", "found alone", time.Now()) })
	class := newClass(testGraph(t, t.TempDir()), learner, "", time.Now())
	asked := make(chan string, 1)
	class.onSwitch = func(said, about string) { asked <- said }
	class.watch("session.output_transcript.delta", "Contame, che.", 1000, 1100)
	class.watch("session.input_transcript.delta", "quiero hablar más rápido, yyy..", 2000, 2500)
	class.watch("session.input_transcript.delta", " about many different topics", 4000, 4500)
	var said string
	select {
	case said = <-asked:
	case <-time.After(switchPause + time.Second):
		t.Fatal("code switch not sent for gap help")
	}
	if !strings.Contains(said, "topics") {
		t.Fatalf("sent a partial turn: %q", said)
	}
	gap := Gap{Said: said, Sentence: "quiero hablar más rápido sobre muchos temas diferentes", Chunks: []GapChunk{
		{English: "about", Spanish: "sobre", Route: "new", Ask: "how do you say about?"},
		{English: "different", Spanish: "diferentes", Also: []string{"diferente"}, Route: "convert", Ask: "how do you say different?"},
		{English: "topics", Spanish: "temas", Route: "import", Guessable: true, Hint: "it's almost like in Norwegian", Ask: "and topics?"},
	}}
	for i, chunk := range gap.Chunks {
		gap.Chunks[i].Known = learner.Known(chunk.Spanish)
	}
	class.SetGap(gap)
	notes := ""
	for len(class.outbox) > 0 {
		notes += (<-class.outbox).content
	}
	if !strings.Contains(notes, "In his dictionary: challenge him") || !strings.Contains(notes, "New but guessable") || !strings.Contains(notes, "New and not guessable: give it") {
		t.Fatalf("notes %s", notes)
	}
	class.watch("session.output_transcript.delta", "Okay, but you know this. How do you say different?", 6000, 6100)
	class.watch("session.input_transcript.delta", "diferentes?", 9000, 9200)
	class.watch("session.output_transcript.delta", "Sí. And topics?", 10000, 10100)
	class.watch("session.input_transcript.delta", "temas", 14000, 14200)
	class.watch("session.output_transcript.delta", "Eso. So what's the end of the sentence?", 15000, 15100)
	class.watch("session.input_transcript.delta", "sobre muchos temas diferentes", 19000, 19500)
	if class.outcomes["gap1/2"] != "alone" || class.outcomes["gap1/3"] != "alone" || class.outcomes["gap1/4"] != "alone" {
		t.Fatalf("outcomes %v", class.outcomes)
	}
	class.Finish("ls_2", time.Now())
	if !learner.Known("temas") || len(learner.data.Gaps) != 3 || learner.data.Gaps[0].Found {
		t.Fatalf("gaps %+v", learner.data.Gaps)
	}
}

func TestCandidates(t *testing.T) {
	dir := t.TempDir()
	graph := testGraph(t, dir)
	learner := OpenLearner("")
	now := time.Now()
	learner.Update(func(l *Learner) {
		l.Thoughts["t02-es"] = ThoughtStatus{Status: "found alone", Alone: 1, At: now.Add(-2 * 24 * time.Hour)}
		l.Thoughts["t04-quiero"] = ThoughtStatus{Status: "found with help", At: now}
	})
	text := Candidates(graph, learner, now)
	if !strings.Contains(text, "- t02-es") || !strings.Contains(text, "- t04-quiero") || !strings.Contains(text, "- t05-yo") {
		t.Fatalf("candidates %s", text)
	}
	learner.Update(func(l *Learner) { l.Thoughts["t02-es"] = ThoughtStatus{Status: "found alone", Alone: 3, At: now} })
	if strings.Contains(Candidates(graph, learner, now), "- t02-es \"") {
		t.Fatal("a thought found alone recently is due")
	}
	if err := graph.Author(Thought{ID: "r-lunfardo", Title: "lunfardo", Needs: []string{"t02-es"}}); err != nil {
		t.Fatal(err)
	}
	if err := graph.Author(Thought{ID: "r-bad", Title: "bad", Needs: []string{"nope"}}); err == nil {
		t.Fatal("dangling prerequisite accepted")
	}
	reloaded, _ := LoadGraph(filepath.Join(dir, "graph"), filepath.Join(dir, "thoughts.jsonl"))
	if thought, ok := reloaded.Get("r-lunfardo"); !ok || thought.Source != "rosa" {
		t.Fatalf("authored thought not saved: %+v", thought)
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
	os.MkdirAll(rosa, 0o700)
	for name, text := range map[string]string{"identity.md": "rosa identity", "live.md": "rosa live", "backend.md": "rosa backend", "voice": "bossa\n"} {
		os.WriteFile(filepath.Join(rosa, name), []byte(text), 0o600)
	}
	testGraph(t, rosa)
	plan, _ := json.Marshal(testPlan())
	os.MkdirAll(filepath.Join(dir, "run", "rosa"), 0o700)
	os.WriteFile(filepath.Join(dir, "run", "rosa", "plan.json"), plan, 0o600)
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
	if err != nil || !strings.Contains(string(data), "session.thinking.append") || !strings.Contains(string(data), "This call's plan") {
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

func TestComposeAndGapPass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
			Text  struct {
				Format struct {
					Name string `json:"name"`
				} `json:"format"`
			} `json:"text"`
			Tools json.RawMessage `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		switch body.Text.Format.Name {
		case "plan":
			plan := `{"mode":"teach","why":"early","items":[{"thought":"t04-quiero","kind":"new","why":"next unlocked","weave":["t02-es"],"targets":[{"en":"I want to compile it","es":"quiero compilarlo","also":[],"note":"","misses":[],"words":[{"word":"quiero","guessable":false,"route":"","hint":""},{"word":"compilarlo","guessable":true,"route":"t04-ation-ar","hint":"compilation"}]},{"en":"","es":"x","also":[],"note":"","misses":[],"words":[]}]},{"thought":"t99-nope","kind":"new","why":"made up","weave":[],"targets":[{"en":"a","es":"b","also":[],"note":"","misses":[],"words":[]}]}]}`
			data, _ := json.Marshal(plan)
			respond(w, `[{"type":"message","content":[{"type":"output_text","text":`+string(data)+`}]}]`)
		case "gap":
			if body.Model != gapModel || body.Tools != nil {
				t.Errorf("gap pass model=%s tools=%s", body.Model, body.Tools)
			}
			gap := `{"sentence":"quiero hablar sobre muchos temas diferentes","chunks":[{"english":"different","spanish":"diferentes","also":[],"route":"convert","thought":"t03-ant-ent","guessable":true,"hint":"think of the -ent words","ask":"how do you say different?","link":"","misses":[]},{"english":"about","spanish":"sobre","also":[],"route":"new","thought":"","guessable":false,"hint":"","ask":"how do you say about?","link":"","misses":[]}],"aside":"tópico means cliché"}`
			data, _ := json.Marshal(gap)
			respond(w, `[{"type":"message","content":[{"type":"output_text","text":`+string(data)+`}]}]`)
		default:
			t.Errorf("unexpected format %q", body.Text.Format.Name)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	testGraph(t, dir)
	r := NewRosa(dir, filepath.Join(dir, "run"))
	r.learner.Update(func(l *Learner) { l.expose("quiero", "found alone", time.Now()) })
	backend := &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: "m", Schema: RosaSchema, Tools: r.Call}
	plan, err := r.compose(context.Background(), backend, composeNext, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || len(plan.Items[0].Targets) != 1 || len(plan.Items[0].Targets[0].Words) != 1 || plan.Items[0].Targets[0].Words[0].Word != "compilarlo" {
		t.Fatalf("plan %+v", plan)
	}
	session := NewSession("ls_gap", &fakeConn{events: make(chan []byte)}, r.memory, &Recall{}, backend, func() string { return "" })
	class := newClass(r.graph, r.learner, "", time.Now())
	gaps := &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: gapModel}
	r.gapPass(session, gaps, class, "quiero hablar about many different topics", "free conversation")
	if class.gap == nil || len(class.gap.Chunks) != 2 || class.gap.Chunks[0].Known || !class.gap.Chunks[0].Guessable {
		t.Fatalf("gap %+v", class.gap)
	}
	session.Close()
}

func TestClassKeepsTheCallMoving(t *testing.T) {
	class := newClass(testGraph(t, t.TempDir()), OpenLearner(""), "", time.Now())
	class.SetPlan(testPlan())
	out := func(text string, ms int64) { class.watch("session.output_transcript.delta", text, ms, ms+100) }
	in := func(text string, ms int64) { class.watch("session.input_transcript.delta", text, ms, ms+100) }
	sent := func() string {
		class.mu.Lock()
		defer class.mu.Unlock()
		text := ""
		for len(class.outbox) > 0 {
			text += (<-class.outbox).content + "\n"
		}
		return text
	}
	sent()

	// The answer's own Spanish in her prompt makes a right answer found with help, not alone.
	class.mu.Lock()
	class.handOver(1)
	class.mu.Unlock()
	sent()
	out("How would you say I want to cancel it? Start with quiero.", 1000)
	in("quiero cancelarlo", 4000)
	if class.outcomes["2:t04-quiero/1"] != "helped" {
		t.Fatalf("outcomes %v", class.outcomes)
	}

	// Feedback with nothing asked, then silence: the box nudges her on.
	idle := idleLimit
	out("Perfect.", 13000)
	time.Sleep(idle + 300*time.Millisecond)
	if text := sent(); !strings.Contains(text, moveOn) {
		t.Fatalf("no nudge after dead air: %s", text)
	}

	// A complete answer over her still-running turn makes her stop and take it.
	out("How would you say it's not normal? And remember where", 20000)
	in("no es normal", 20200)
	if text := sent(); !strings.Contains(text, takeAnswer) {
		t.Fatalf("no take-over: %s", text)
	}

	// An uncued attempt lets go of a target that has no correction under way.
	out("Tell me about your day.", 30000)
	in("I coded all day", 33000)
	if class.armed != nil {
		t.Fatalf("still armed: %v", class.armed)
	}
}

func TestClassHearsClarificationsAndCorrections(t *testing.T) {
	if judge("Uh constant mente", Sentence{EN: "constantly", ES: "constantemente"}) == verdictRight {
		t.Error("a dropped vowel judged right")
	}
	for _, said := range []string{"Uma young It's really different", "sorry, what was it?", "it's really different?"} {
		if v := judge(said, Sentence{EN: "it's really different", ES: "es realmente diferente"}); v != verdictClarify {
			t.Errorf("judge(%q) = %s, want clarify", said, v)
		}
	}
	if finished("Eso, constante. And then") || finished("Eso, so.") || !finished("Eso, constante.") {
		t.Error("finished")
	}
	class := newClass(testGraph(t, t.TempDir()), OpenLearner(""), "", time.Now())
	class.SetPlan(testPlan())
	for len(class.outbox) > 0 {
		<-class.outbox
	}
	out := func(text string, ms int64) { class.watch("session.output_transcript.delta", text, ms, ms+100) }
	in := func(text string, ms int64) { class.watch("session.input_transcript.delta", text, ms, ms+100) }
	out("How would you say it's not normal?", 1000)
	in("Sorry, it's not normal?", 3000)
	time.Sleep(settleDelay + 100*time.Millisecond)
	class.mu.Lock()
	notes := ""
	for len(class.outbox) > 0 {
		notes += (<-class.outbox).content
	}
	class.mu.Unlock()
	out("It's not normal. No es normal is what I want from you.", 4000)
	if !strings.Contains(notes, sayAgain) || class.outcomes["1:t02-es/2"] != "" || class.misses["1:t02-es/2"] != 0 {
		t.Fatalf("clarification treated as an answer: notes %q outcomes %v misses %v", notes, class.outcomes, class.misses)
	}
	out(" So, how would you say it's not normal?", 5000)
	in("no es normal", 8000)
	out("No, don't rush it. Again.", 9000)
	if class.outcomes["1:t02-es/2"] != "helped" {
		t.Fatalf("Rosa's correction ignored: %v", class.outcomes)
	}
}
