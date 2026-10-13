package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// overloaded is what the Codex endpoint streams when it fails on its side.
func overloaded(w http.ResponseWriter) {
	fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"Our servers are currently overloaded. Please try again later.\"}}}\n\n")
}

func instantRetries(t *testing.T) {
	saved := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = saved })
}

func TestBackendResendsWhatTheEndpointFailedWithoutRerunningTools(t *testing.T) {
	instantRetries(t)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1, 3:
			overloaded(w)
		case 2:
			respond(w, `[{"type":"function_call","call_id":"c1","name":"ask_project","arguments":"{}"}]`)
		case 4:
			w.WriteHeader(http.StatusBadGateway)
		default:
			respond(w, `[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"Vanta check is queued.\",\"details\":\"\"}"}]}]`)
		}
	}))
	defer server.Close()
	ran := 0
	backend := &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: "m",
		Tools: func(context.Context, string, string) (any, error) {
			ran++
			return map[string]string{"status": "queued"}, nil
		}}
	reply, err := backend.Answer(context.Background(), []map[string]any{seedMessage("user", "check vanta")})
	if err != nil || reply.Say != "Vanta check is queued." {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
	if ran != 1 || requests != 5 {
		t.Fatalf("tool ran %d times over %d requests", ran, requests)
	}
}

func TestBackendDoesNotResendARejectedRequest(t *testing.T) {
	instantRetries(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"detail":"bad input"}`)
	}))
	defer server.Close()
	backend := &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: "m"}
	if _, err := backend.Answer(context.Background(), nil); err == nil || Transient(err) || requests != 1 {
		t.Fatalf("err=%v transient=%v requests=%d", err, Transient(err), requests)
	}
}

// A delegation that fails tells the call the real error and what did or did not start, and the next
// delegation sees that failure instead of concluding nothing failed.
func TestFailedDelegationIsSaidPreciselyAndRecalled(t *testing.T) {
	instantRetries(t)
	var mu sync.Mutex
	var inputs []string
	healthy := false
	toolFirst := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input json.RawMessage `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		inputs = append(inputs, string(body.Input))
		switch {
		case healthy:
			respond(w, `[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"My first reply failed: the backend was overloaded.\",\"details\":\"\"}"}]}]`)
		case toolFirst && !strings.Contains(string(body.Input), "function_call_output"):
			respond(w, `[{"type":"function_call","call_id":"c1","name":"ask_project","arguments":"{\"project_path\":\"/home/lemon/git/journalia/journalia\"}"}]`)
		default:
			overloaded(w)
		}
	}))
	defer server.Close()
	conn := &fakeConn{events: make(chan []byte, 16)}
	recall := &Recall{}
	backend := &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: "m",
		Tools: func(context.Context, string, string) (any, error) {
			return map[string]any{"pane": "Journalia", "ticket": 6, "status": "Journalia is queued; it starts as soon as the session is ready and progress arrives on its own. Do not wait or poll."}, nil
		}}
	session := NewSession("ls_1", conn, OpenMemory(""), recall, backend, func() string { return "Dispatcher state." })
	go session.Run()
	written := func(n int) []map[string]any {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			conn.mu.Lock()
			wrote := append([]map[string]any(nil), conn.wrote...)
			conn.mu.Unlock()
			if len(wrote) >= n {
				return wrote
			}
		}
		t.Fatalf("waited for %d writes", n)
		return nil
	}
	delegate := func(said, id string) {
		conn.events <- []byte(`{"type":"session.input_transcript.delta","delta":"` + said + `"}`)
		conn.events <- []byte(`{"type":"session.delegation.created","delegation":{"id":"` + id + `","target":"client"}}`)
	}

	delegate("check vanta in journalia", "item_1")
	started := written(1)[0]["content"].(string)
	if started != "Done before my reply failed: Journalia has the instruction queued as ticket 6. My reply failed: backend failed: Our servers are currently overloaded. Please try again later. The box resent only the reply request, 2 times, never an instruction. Nothing needs sending again." {
		t.Fatalf("said=%q", started)
	}

	mu.Lock()
	toolFirst = false
	mu.Unlock()
	delegate("save how to say journalia", "item_2")
	nothing := written(2)[1]["content"].(string)
	if !strings.Contains(nothing, "Our servers are currently overloaded") || !strings.HasSuffix(nothing, "No tool ran, so nothing was started or changed.") {
		t.Fatalf("said=%q", nothing)
	}

	mu.Lock()
	healthy = true
	mu.Unlock()
	delegate("what failed", "item_3")
	answer := written(3)[2]
	mu.Lock()
	last := inputs[len(inputs)-1]
	mu.Unlock()
	if !strings.Contains(last, `save how to say journalia\nThe backend's reply to this request failed, and Lemon was told: My reply failed: backend failed: Our servers are currently overloaded`) ||
		!strings.Contains(last, "No tool ran, so nothing was started or changed.") {
		t.Fatalf("the next delegation cannot see the failure: %s", last)
	}
	if answer["delegation_id"] != "item_3" || !strings.Contains(answer["content"].(string), "overloaded") {
		t.Fatalf("answer=%v", answer)
	}
	conn.events <- []byte(`{"type":"session.closed","reason":"close_requested"}`)
	<-session.Closed
}

// While a delegation runs, the voice model hears once and quietly that it is running, before the
// result, and nothing more: repeated notes became spoken "still going" filler. Rosa's planner
// delegations get no note.
func TestRunningDelegationGetsOneQuietNote(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		respond(w, `[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"Ticket 6 is queued in Journalia.\",\"details\":\"\"}"}]}]`)
	}))
	defer server.Close()
	backend := &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: "m"}
	for _, agent := range []string{"carla", "rosa"} {
		conn := &fakeConn{events: make(chan []byte, 8)}
		session := NewSession("ls_1", conn, OpenMemory(""), &Recall{}, backend, func() string { return "" })
		session.Agent = agent
		go session.Run()
		conn.events <- []byte(`{"type":"session.input_transcript.delta","delta":"leave it ready in the pane"}`)
		conn.events <- []byte(`{"type":"session.delegation.created","delegation":{"id":"item_1","target":"client"}}`)
		time.Sleep(1500 * time.Millisecond) // a long wait for the backend
		release <- struct{}{}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			conn.mu.Lock()
			n := len(conn.wrote)
			conn.mu.Unlock()
			if n > 0 {
				break
			}
		}
		conn.events <- []byte(`{"type":"session.closed","reason":"close_requested"}`)
		<-session.Closed
		conn.mu.Lock()
		running, wrote := conn.running, conn.wrote
		conn.mu.Unlock()
		if len(wrote) != 1 || wrote[0]["content"] != "Ticket 6 is queued in Journalia." {
			t.Fatalf("%s wrote=%v", agent, wrote)
		}
		if agent == "rosa" {
			if len(running) != 0 {
				t.Fatalf("rosa got running notes: %v", running)
			}
			continue
		}
		if len(running) != 1 || running[0]["delegation_id"] != nil {
			t.Fatalf("running=%v", running)
		}
		var note, result int
		fmt.Sscanf(running[0]["event_id"].(string), "box_%d", &note)
		fmt.Sscanf(wrote[0]["event_id"].(string), "box_%d", &result)
		if note > result {
			t.Fatalf("the running note followed the result: note %d, result %d", note, result)
		}
	}
}

// The endpoint's failure events carry their reason in several places; an empty one is logged raw.
func TestBackendFailureNamesWhatTheEndpointSaid(t *testing.T) {
	instantRetries(t)
	for event, want := range map[string]string{
		`{"type":"error","error":{"type":"server_error","code":"server_is_overloaded","message":"Try again later."}}`: "backend failed: server_error: server_is_overloaded: Try again later.",
		`{"type":"error","code":"rate_limit_exceeded","message":"Slow down."}`:                                        "backend failed: rate_limit_exceeded: Slow down.",
		`{"type":"response.failed","response":{"error":{"code":"server_error","message":""}}}`:                        "backend failed: server_error",
		`{"type":"error","sequence_number":4}`:                                                                        `backend failed: the endpoint gave no reason: {"type":"error","sequence_number":4}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}))
		backend := &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: "m"}
		_, err := backend.Answer(context.Background(), nil)
		server.Close()
		if err == nil || err.Error() != want || !Transient(err) {
			t.Errorf("%s: err=%v", event, err)
		}
	}
}

// A failed lookup tells the backend model that only the lookup failed and looking again is safe; a
// failed mutation says not to retry.
func TestToolErrorsSayWhatFailed(t *testing.T) {
	var outputs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []map[string]any `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		outputs = nil
		for _, item := range body.Input {
			if item["type"] == "function_call_output" {
				outputs = append(outputs, item["output"].(string))
			}
		}
		switch len(outputs) {
		case 0:
			respond(w, `[{"type":"function_call","call_id":"c1","name":"check_pane","arguments":"{\"pane_id\":\"carla-mobile\"}"}]`)
		case 1:
			respond(w, `[{"type":"function_call","call_id":"c2","name":"close_pane","arguments":"{\"pane_id\":\"%1\"}"}]`)
		default:
			respond(w, `[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"ok\",\"details\":\"\"}"}]}]`)
		}
	}))
	defer server.Close()
	d := NewDispatcher(&fakeBox{panes: []Pane{{ID: "%1", Label: "Voice Architecture Reassessment", Bridge: true}}}, NewDiscord(func(Update) {}), func(Update) {})
	backend := &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: "m",
		Tools: func(ctx context.Context, name, arguments string) (any, error) {
			if name == "close_pane" {
				return nil, fmt.Errorf("tmux timed out")
			}
			return d.Call(ctx, name, arguments)
		}}
	if _, err := backend.Answer(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(outputs) < 2 {
		t.Fatalf("outputs=%v", outputs)
	}
	if !strings.Contains(outputs[0], `no open session has the id or task name \"carla-mobile\"`) || !strings.Contains(outputs[0], "Only this lookup failed: nothing changed and no session's work failed. Looking again is safe.") {
		t.Fatalf("lookup=%s", outputs[0])
	}
	if !strings.Contains(outputs[1], "tmux timed out. Do not retry automatically; it may have taken effect.") {
		t.Fatalf("mutation=%s", outputs[1])
	}
}

// 13 Oct: ask_project queued the work, then the reply request failed with an empty error event, and
// the call heard "That request failed on the box" over a successful enqueue. A queued instruction is
// done; only the reply failed, only the reply request was resent, and the tool ran once.
func TestQueuedThenReplyFailureIsSaidAsDone(t *testing.T) {
	instantRetries(t)
	var mu sync.Mutex
	failing := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input json.RawMessage `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case !strings.Contains(string(body.Input), "function_call_output"):
			respond(w, `[{"type":"function_call","call_id":"c1","name":"ask_project","arguments":"{\"project_path\":\"/home/lemon/git/ee/quill\",\"text\":\"fix it\",\"mode\":\"narrate\"}"}]`)
		case failing:
			fmt.Fprint(w, "data: {\"type\":\"error\",\"sequence_number\":7}\n\n")
		default:
			respond(w, `[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"Queued in quill.\",\"details\":\"\"}"}]}]`)
		}
	}))
	defer server.Close()
	box := &fakeBox{transcripts: map[string][]json.RawMessage{}}
	d := NewDispatcher(box, NewDiscord(func(Update) {}), func(Update) {})
	backend := &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: "m", Tools: d.Call, Mutates: Mutates}

	reply, err := backend.Answer(context.Background(), nil)
	if err == nil || len(reply.Fetched) != 1 {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
	said := Failure(err, reply.Fetched)
	want := "Done before my reply failed: quill has the instruction queued as ticket 1. My reply failed: backend failed: the endpoint gave no reason: {\"type\":\"error\",\"sequence_number\":7}. The box resent only the reply request, 2 times, never an instruction. Nothing needs sending again."
	if said != want {
		t.Fatalf("said=%q", said)
	}
	if strings.Contains(said, "on the box") || len(box.spawned) != 1 {
		t.Fatalf("said=%q spawned=%v", said, box.spawned)
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	reply, err = backend.Answer(context.Background(), nil)
	if err != nil || reply.Say != "Queued in quill." || !strings.Contains(reply.Fetched[0].Result, `"status":"quill is queued`) {
		t.Fatalf("a queued enqueue is a plain answer: reply=%+v err=%v", reply, err)
	}
}

// One answer runs a change with the same arguments once, even when the model asks again.
func TestAnswerNeverRepeatsAChange(t *testing.T) {
	call := `{"type":"function_call","call_id":"c%d","name":"%s","arguments":"{\"pane_id\":\"%%1\",\"text\":\"go\",\"mode\":\"narrate\"}"}`
	var outputs []string
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []map[string]any `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		outputs = nil
		for _, item := range body.Input {
			if item["type"] == "function_call_output" {
				outputs = append(outputs, item["output"].(string))
			}
		}
		requests++
		switch requests {
		case 1, 2:
			respond(w, "["+fmt.Sprintf(call, requests, "ask_pane")+","+fmt.Sprintf(call, 10+requests, "check_pane")+"]")
		default:
			respond(w, `[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"ok\",\"details\":\"\"}"}]}]`)
		}
	}))
	defer server.Close()
	ran := map[string]int{}
	backend := &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: "m", Mutates: Mutates,
		Tools: func(_ context.Context, name, _ string) (any, error) {
			ran[name]++
			return map[string]string{"result": "ok"}, nil
		}}
	reply, err := backend.Answer(context.Background(), nil)
	if err != nil || ran["ask_pane"] != 1 || ran["check_pane"] != 2 || len(reply.Fetched) != 3 {
		t.Fatalf("ran=%v fetched=%+v err=%v", ran, reply.Fetched, err)
	}
	if len(outputs) != 4 || !strings.Contains(outputs[2], "already ran in this answer and was not repeated") {
		t.Fatalf("outputs=%v", outputs)
	}
}

// A failed lookup changed nothing; a failed change may have taken effect and is not repeated.
func TestFailureScopesEachTool(t *testing.T) {
	said := Failure(fmt.Errorf("backend returned HTTP 400: bad input"), []Fetched{
		{"check_pane", `{"pane_id":"carla-mobile"}`, `{"error":"no open session has the id or task name \"carla-mobile\"; call list_panes for the current ones. Only this lookup failed: nothing changed and no session's work failed. Looking again is safe."}`},
		{"spawn_pane", `{}`, `{"error":"tmux timed out. Do not retry automatically; it may have taken effect."}`},
	})
	want := `The check_pane lookup failed and changed nothing: no open session has the id or task name "carla-mobile"; call list_panes for the current ones; spawn_pane failed: tmux timed out; it may still have taken effect, so it was not repeated. My reply failed: backend returned HTTP 400: bad input.`
	if said != want {
		t.Fatalf("said=%q", said)
	}
}
