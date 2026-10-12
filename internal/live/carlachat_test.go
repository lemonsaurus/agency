package live

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCarlaChatRunsCallToolsAndTextsTheUpdates(t *testing.T) {
	paced = func(time.Duration) time.Duration { return 0 }
	defer func() { paced = func(d time.Duration) time.Duration { return d } }()
	var mu sync.Mutex
	var instructions string
	var inputs []string
	say := func(w http.ResponseWriter, text string) {
		data, _ := json.Marshal(`{"say":"` + text + `","details":"quill session asked for status"}`)
		respond(w, `[{"type":"message","content":[{"type":"output_text","text":`+string(data)+`}]}]`)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Instructions string          `json:"instructions"`
			Input        json.RawMessage `json:"input"`
			Tools        []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for _, tool := range body.Tools {
			if tool.Name == "conversation" || strings.HasPrefix(tool.Name, "discord") {
				t.Errorf("chat offers %s", tool.Name)
			}
		}
		input := string(body.Input)
		mu.Lock()
		instructions = body.Instructions
		inputs = append(inputs, input)
		mu.Unlock()
		switch {
		case strings.Contains(input, "function_call_output"):
			say(w, "On it, quill is checking.")
		case strings.Contains(input, "Answer Lemon's latest messages now."):
			respond(w, `[{"type":"function_call","call_id":"c1","name":"ask_project","arguments":"{\"project_path\":\"/home/lemon/git/ee/quill\",\"text\":\"status?\",\"mode\":\"narrate\"}"}]`)
		default:
			say(w, "Quill says it is done.")
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "live.md"), []byte("live rules"), 0o600)
	os.WriteFile(filepath.Join(dir, "backend.md"), []byte("backend rules"), 0o600)
	os.WriteFile(filepath.Join(dir, "chat.md"), []byte("texting rules"), 0o600)
	box := &fakeBox{transcripts: map[string][]json.RawMessage{}}
	m := NewManager(box, "key", func() (string, error) { return "persona", nil }, dir, filepath.Join(dir, "run", "memory.jsonl"))
	backend := m.carlaChat.Backend
	m.carlaChat.Backend = func() (*Backend, error) {
		b, err := backend()
		if err == nil {
			b.Client, b.URL, b.Auth = server.Client(), server.URL, testAuth
		}
		return b, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.carlaChat.Run(ctx)
	if _, err := m.Handle(ctx, `{"op":"chat-send","agent":"carla","text":"how is quill doing?"}`); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		reply, _ := m.Handle(ctx, `{"op":"chat-sync","agent":"carla","rev":0}`)
		var sync ChatSync
		json.Unmarshal([]byte(reply), &sync)
		texts = nil
		for _, message := range sync.Messages {
			if message.From == "carla" {
				texts = append(texts, message.Text)
			}
		}
		if len(texts) >= 2 && sync.Doing == "" {
			if sync.Messages[0].Read == 0 {
				t.Errorf("his message was not marked read")
			}
			break
		}
	}
	if len(texts) < 2 || texts[0] != "On it, quill is checking." || texts[len(texts)-1] != "Quill says it is done." {
		t.Fatalf("texts=%q", texts)
	}
	mu.Lock()
	if !strings.HasPrefix(instructions, "persona\n\n# Voice backend\n\nbackend rules") || !strings.HasSuffix(instructions, "# Texting\n\ntexting rules") {
		t.Errorf("instructions=%q", instructions)
	}
	if last := inputs[len(inputs)-1]; !strings.Contains(last, "Updates from the sessions working on what he texted for") || !strings.Contains(last, "has started on: status?") {
		t.Errorf("updates not in input: %s", last)
	}
	mu.Unlock()
	if tickets := m.dispatcher.Tickets(); len(tickets) != 1 || !tickets[0].Texted {
		t.Fatalf("tickets=%+v", tickets)
	}
	if pending := m.Status().Pending; pending != 0 {
		t.Fatalf("chat updates went to the call: %d pending", pending)
	}
	if brief := m.recall.Brief(time.Now()); !strings.Contains(brief, "Lemon said: how is quill doing?") {
		t.Fatalf("the call does not recall the chat: %s", brief)
	}
	rosa, _ := m.Handle(ctx, `{"op":"chat-sync","rev":0}`)
	if strings.Contains(rosa, "quill") {
		t.Fatalf("Carla's chat leaked into Rosa's: %s", rosa)
	}
}

func TestCarlaChatOnlyRunsItsTools(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(&fakeBox{transcripts: map[string][]json.RawMessage{}}, "key", func() (string, error) { return "persona", nil }, dir, filepath.Join(dir, "memory.jsonl"))
	for _, name := range []string{"conversation", "discord_recent", "discord_reply", "teach"} {
		if _, err := m.text(context.Background(), name, `{"state":"off","id":1,"text":"yes"}`); err == nil {
			t.Errorf("%s ran from the chat", name)
		}
	}
	if m.Status().Phone != "" {
		t.Fatal("the chat turned the call off")
	}
	if _, err := m.text(context.Background(), "remind", `{"at":"+20m","text":"stretch"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Handle(context.Background(), `{"op":"chat-send","agent":"bob","text":"hi"}`); err == nil {
		t.Fatal("unknown agent accepted")
	}
	if _, err := m.Handle(context.Background(), `{"op":"chat-react","agent":"carla","id":1,"emoji":"👍"}`); err == nil {
		t.Fatal("reacted to a message that does not exist")
	}
}

func TestCarlaChatQuietUpdatesWaitForHerNextText(t *testing.T) {
	chat := OpenCarlaChat(t.TempDir())
	chat.Report(Update{false, "Quill is working on this in the background."})
	if chat.spoken() {
		t.Fatal("a quiet update asked for a text")
	}
	chat.Report(Update{true, "Quill finished the background task."})
	if !chat.spoken() {
		t.Fatal("a spoken update did not ask for a text")
	}
	input, _ := json.Marshal(chat.input(time.Now(), chat.news, "close"))
	if !strings.Contains(string(input), "(context): Quill is working") || !strings.Contains(string(input), "(for him): Quill finished") {
		t.Fatalf("input=%s", input)
	}
}
