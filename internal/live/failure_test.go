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
			respond(w, `[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"The first try failed on the box: the backend was overloaded.\",\"details\":\"\"}"}]}]`)
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
			return map[string]string{"status": "Journalia is queued"}, nil
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
	if started != "That request failed on the box: backend failed: Our servers are currently overloaded. Please try again later. The box already resent it, and it failed every time. Before it failed: ask_project returned {\"status\":\"Journalia is queued\"}." {
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
	if !strings.Contains(last, `save how to say journalia\nThis request failed on the box, and Lemon was told: That request failed on the box: backend failed: Our servers are currently overloaded`) ||
		!strings.Contains(last, "No tool ran, so nothing was started or changed.") {
		t.Fatalf("the next delegation cannot see the failure: %s", last)
	}
	if answer["delegation_id"] != "item_3" || !strings.Contains(answer["content"].(string), "overloaded") {
		t.Fatalf("answer=%v", answer)
	}
	conn.events <- []byte(`{"type":"session.closed","reason":"close_requested"}`)
	<-session.Closed
}

// While a delegation runs, the voice model hears that it is running and has not failed, before any
// result and never after it; Rosa's planner delegations get no such notes.
func TestRunningDelegationIsNeverSilence(t *testing.T) {
	saved := runningEvery
	runningEvery = 20 * time.Millisecond
	defer func() { runningEvery = saved }()
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
		time.Sleep(550 * time.Millisecond) // past the delegation's 400 ms wait for late transcript
		release <- struct{}{}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			conn.mu.Lock()
			n := len(conn.wrote)
			conn.mu.Unlock()
			if n > 0 {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
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
		if len(running) < 3 || !strings.Contains(running[0]["content"].(string), "Nothing has failed.") || running[0]["delegation_id"] != nil {
			t.Fatalf("running=%v", running)
		}
		// box_N counts every write; no note may follow the result.
		var last, result int
		fmt.Sscanf(running[len(running)-1]["event_id"].(string), "box_%d", &last)
		fmt.Sscanf(wrote[0]["event_id"].(string), "box_%d", &result)
		if last > result {
			t.Fatalf("a running note followed the result: note %d, result %d", last, result)
		}
	}
}
