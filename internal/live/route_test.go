package live

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 27 Nov: Lemon asked whether Carla could see his Obsidian notes and ask_project was aimed at a path
// that is not a project. The call answered "routing error" and offered to send it again; the retry,
// aimed at a real project, worked. A path off the box's list fails before anything starts, tells the
// model so, and permits a corrected call.
func TestAskProjectOutsideTheProjectListFailsBeforeAnythingStarts(t *testing.T) {
	box := &fakeBox{transcripts: map[string][]json.RawMessage{}}
	d := NewDispatcher(box, NewDiscord(func(Update) {}), func(Update) {})
	_, err := d.Call(context.Background(), "ask_project", `{"project_path":"/home/lemon/notes/LemonsaurusOnline","text":"can you read the vault?","mode":"narrate"}`)
	var route RouteError
	if !errors.As(err, &route) {
		t.Fatalf("err = %v, want a RouteError", err)
	}
	if !strings.Contains(err.Error(), "notes vault or file path goes in text") {
		t.Fatalf("err = %v", err)
	}
	if len(box.spawned) != 0 || len(box.sent) != 0 || len(d.Tickets()) != 0 {
		t.Fatalf("something started: spawned=%v sent=%v tickets=%v", box.spawned, box.sent, d.Tickets())
	}

	var requests int
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			respond(w, `[{"type":"function_call","call_id":"c1","name":"ask_project","arguments":"{\"project_path\":\"/home/lemon/notes\",\"text\":\"vault?\",\"mode\":\"narrate\"}"}]`)
			return
		}
		body := make([]byte, 1<<16)
		n, _ := r.Body.Read(body)
		seen = string(body[:n])
		respond(w, `[{"type":"message","content":[{"type":"output_text","text":"{\"say\":\"ok\",\"details\":\"\"}"}]}]`)
	}))
	defer server.Close()
	backend := &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: "m", Tools: d.Call}
	if _, err := backend.Answer(context.Background(), []map[string]any{seedMessage("user", "notes?")}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "Nothing was started and nothing was sent") || strings.Contains(seen, "may have taken effect") {
		t.Fatalf("the model was told: %s", seen)
	}
}
