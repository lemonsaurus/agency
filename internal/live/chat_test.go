package live

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatRepliesLikeAPerson(t *testing.T) {
	paced = func(time.Duration) time.Duration { return 0 }
	defer func() { paced = func(d time.Duration) time.Duration { return d } }()
	var seen atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Instructions string          `json:"instructions"`
			Input        json.RawMessage `json:"input"`
			Tools        []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		seen.Store(string(body.Input))
		for _, tool := range body.Tools {
			if tool.Name == "teach" || tool.Name == "conversation" {
				t.Errorf("chat offers call tool %s", tool.Name)
			}
		}
		reply := `{"react":{"id":1,"emoji":"😂"},"messages":[{"kind":"text","text":"jajaja","scene":"","selfie":false},{"kind":"voice","text":"che, escuchá","scene":"","selfie":false},{"kind":"photo","text":"mirá","scene":"the Corsa at night","selfie":false},{"kind":"text","text":"¿y vos?","scene":"","selfie":false}]}`
		data, _ := json.Marshal(reply)
		respond(w, `[{"type":"message","content":[{"type":"output_text","text":`+string(data)+`}]}]`)
	}))
	defer server.Close()
	dir := t.TempDir()
	testGraph(t, dir)
	os.WriteFile(filepath.Join(dir, "identity.md"), []byte("Rosa."), 0o600)
	os.WriteFile(filepath.Join(dir, "chat.md"), []byte("Texting."), 0o600)
	r := NewRosa(dir, filepath.Join(dir, "run"))
	r.memory.Add("assistant", "¿cómo se dice I want?", time.Now().Add(-time.Hour))
	chat := OpenChat(r)
	r.Chat = chat
	woke := make(chan struct{}, 8)
	chat.Wake = func() { woke <- struct{}{} }
	chat.Backend = func() *Backend {
		instructions, _ := r.ChatInstructions()
		return &Backend{Client: server.Client(), URL: server.URL, Auth: testAuth, Model: "m", Instructions: instructions, Schema: ChatSchema, Tools: r.Call}
	}
	chat.Record = func(ctx context.Context, script string) (VoiceNote, error) {
		return VoiceNote{WAV: []byte("RIFF"), Duration: 2 * time.Second, Peaks: []float64{.5, 1}}, nil
	}
	chat.Take = func(ctx context.Context, scene string, look []byte) ([]byte, error) {
		if look != nil || !strings.Contains(scene, "the Corsa at night") {
			t.Errorf("photo scene=%q look=%v", scene, look != nil)
		}
		return []byte("jpeg"), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go chat.Run(ctx)
	if _, err := chat.Send("quiero un choripán"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var sync ChatSync
	for time.Now().Before(deadline) {
		if sync = chat.Sync(0); len(sync.Messages) == 5 && sync.Doing == "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(sync.Messages) != 5 {
		t.Fatalf("messages %+v", sync.Messages)
	}
	mine, voice, photo := sync.Messages[0], sync.Messages[2], sync.Messages[3]
	if mine.Read == 0 || mine.Reactions["rosa"] != "😂" || voice.Kind != "voice" || voice.Seconds != 2 || photo.Kind != "photo" || photo.Text != "mirá" {
		t.Fatalf("mine=%+v voice=%+v photo=%+v", mine, voice, photo)
	}
	if data, _ := os.ReadFile(voice.Media); string(data) != "RIFF" {
		t.Fatalf("voice note file %q", data)
	}
	if input, _ := seen.Load().(string); !strings.Contains(input, "quiero un choripán") || !strings.Contains(input, "¿cómo se dice I want?") {
		t.Fatalf("input lacks the chat or the last call: %s", input)
	}
	if len(woke) != 0 {
		t.Fatalf("pushed while the chat was open")
	}
	if later := chat.Sync(sync.Rev); len(later.Messages) != 0 || !later.Online {
		t.Fatalf("sync after rev %+v", later)
	}
	chat.React(2, "lemon", "❤️")
	if changed := chat.Sync(sync.Rev); len(changed.Messages) != 1 || changed.Messages[0].Reactions["lemon"] != "❤️" {
		t.Fatalf("reaction %+v", changed)
	}
	reopened := OpenChat(r)
	if len(reopened.messages) != 5 || reopened.state.Handled != 1 {
		t.Fatalf("reopened %d messages, handled %d", len(reopened.messages), reopened.state.Handled)
	}
	if seed, _ := json.Marshal(r.Seed(time.Now())); !strings.Contains(string(seed), "WhatsApp texts") || !strings.Contains(string(seed), "(voice note) che, escuchá") {
		t.Fatalf("a call does not know the texts: %s", seed)
	}
}

func TestVoiceNoteEncoding(t *testing.T) {
	pcm := make([]byte, noteRate*2*2)
	for i := noteRate / 2; i < noteRate*3/2; i++ {
		pcm[i*2+1] = 0x20
	}
	note := encodeNote(pcm)
	if note.Duration < time.Second || note.Duration > 1500*time.Millisecond || len(note.Peaks) != 48 || string(note.WAV[:4]) != "RIFF" || string(note.WAV[8:16]) != "WAVEfmt " {
		t.Fatalf("duration %s, %d peaks, header %q", note.Duration, len(note.Peaks), note.WAV[:16])
	}
	if covered("che, Lemon, escuchá", "Che Lemon escuchá esto") != .75 {
		t.Fatalf("coverage %v", covered("che, Lemon, escuchá", "Che Lemon escuchá esto"))
	}
}

func TestChatTakesHisVoiceNotes(t *testing.T) {
	dir := t.TempDir()
	r := NewRosa(dir, filepath.Join(dir, "run"))
	chat := OpenChat(r)
	chat.Hear = func(ctx context.Context, path string) (string, error) { return "quiero hablar about music", nil }
	if _, err := chat.SendVoice(context.Background(), filepath.Join(dir, "elsewhere.m4a"), 3, nil); err == nil {
		t.Fatal("took a voice note from outside the chat folder")
	}
	os.MkdirAll(chat.Dir(), 0o700)
	path := filepath.Join(chat.Dir(), "lemon-1.m4a")
	os.WriteFile(path, []byte("m4a"), 0o600)
	message, err := chat.SendVoice(context.Background(), path, 3.2, []float64{.2, 1})
	if err != nil || message.Kind != "voice" || message.From != "lemon" || message.Text != "quiero hablar about music" || message.Seconds != 3.2 {
		t.Fatalf("message %+v, %v", message, err)
	}
	if input, _ := json.Marshal(chat.input(time.Now(), "reply")); !strings.Contains(string(input), "speech recognition of what he said) quiero hablar about music") {
		t.Fatalf("input %s", input)
	}
}
