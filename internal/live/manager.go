package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Manager is the box side of Carla's voice: it creates Live sessions for the phone, attaches the
// sideband, and keeps the dispatcher, Discord relay and memory alive between sessions.
type Manager struct {
	box        Box
	key        string
	persona    func() (string, error)
	prompts    string
	memory     *Memory
	dispatcher *Dispatcher
	discord    *Discord
	client     *http.Client
	API        string

	mu      sync.Mutex
	session *Session
	pending []Update
	said    []string
	phone   string
}

func NewManager(box Box, key string, persona func() (string, error), promptDir, memoryPath string) *Manager {
	m := &Manager{box: box, key: key, persona: persona, prompts: promptDir, memory: OpenMemory(memoryPath), client: &http.Client{Timeout: 120 * time.Second}, API: "https://api.openai.com"}
	m.discord = NewDiscord(m.Emit)
	m.dispatcher = NewDispatcher(box, m.discord, m.Emit)
	return m
}

type startRequest struct {
	Voice  string `json:"voice"`
	Accent string `json:"accent"`
	SDP    string `json:"sdp"`
}

// Start creates the Live session for the phone's SDP offer, attaches the sideband, and returns
// the creation reply (session id and SDP answer) for the phone.
func (m *Manager) Start(ctx context.Context, request startRequest) (string, error) {
	if m.key == "" {
		return "", fmt.Errorf("OPENAI_API_KEY is not set on the box")
	}
	if request.SDP == "" || request.Voice == "" {
		return "", fmt.Errorf("voice and sdp are required")
	}
	live, backend, err := m.instructions(request.Accent)
	if err != nil {
		return "", err
	}
	config := map[string]any{
		"model":        "gpt-live-1",
		"instructions": live,
		"audio":        map[string]any{"output": map[string]any{"voice": request.Voice}},
		"delegation":   map[string]any{"type": "client"},
	}
	if seed := m.memory.Seed(time.Now()); len(seed) > 0 {
		config["input"] = seed
	}
	body, _ := json.Marshal(map[string]any{"session": config, "transport": map[string]any{"type": "webrtc", "sdp": request.SDP}})
	reply, err := post(ctx, m.client, m.API+"/v1/live/sessions", m.key, body)
	if err != nil {
		return "", err
	}
	var answer struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if json.Unmarshal(reply, &answer) != nil || answer.Session.ID == "" {
		return "", fmt.Errorf("invalid live session reply")
	}
	if err := m.attach(answer.Session.ID, backend); err != nil {
		return "", err
	}
	return string(reply), nil
}

func (m *Manager) instructions(accent string) (string, string, error) {
	persona, err := m.persona()
	if err != nil {
		return "", "", err
	}
	live, err := os.ReadFile(filepath.Join(m.prompts, "live.md"))
	if err != nil {
		return "", "", fmt.Errorf("cannot read voice/live.md from ~/.agents")
	}
	backend, err := os.ReadFile(filepath.Join(m.prompts, "backend.md"))
	if err != nil {
		return "", "", fmt.Errorf("cannot read voice/backend.md from ~/.agents")
	}
	spoken := ""
	if strings.TrimSpace(accent) != "" {
		spoken = fmt.Sprintf("\n\nSpeak %s English with a natural, contemporary %s accent throughout.", accent, accent)
	}
	return persona + "\n\n" + strings.TrimSpace(string(live)) + spoken, persona + "\n\n# Voice backend\n\n" + strings.TrimSpace(string(backend)), nil
}

func (m *Manager) attach(id string, instructions string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	url := strings.Replace(m.API, "http", "ws", 1) + "/v1/live/sessions/" + id + "/attach"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + m.key}}, HTTPClient: m.client})
	if err != nil {
		return fmt.Errorf("sideband attach failed: %v", err)
	}
	conn.SetReadLimit(8 << 20)
	backend := &Backend{Client: m.client, URL: m.API + "/v1/responses", Key: m.key, Model: "gpt-5.6-terra", Instructions: instructions, Tools: m.call}
	session := NewSession(id, wsConn{conn}, m.memory, backend, m.state)
	m.mu.Lock()
	previous := m.session
	m.session = session
	pending := m.pending
	m.pending = nil
	m.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	go func() {
		session.Run()
		m.mu.Lock()
		if m.session == session {
			m.session = nil
		}
		m.mu.Unlock()
	}()
	for _, update := range pending {
		session.Emit(update)
	}
	return nil
}

// Emit routes an update to the live session, or holds spoken ones for the next session.
func (m *Manager) Emit(update Update) {
	m.mu.Lock()
	session := m.session
	if session == nil && update.Spoken {
		m.pending = append(m.pending, update)
		if len(m.pending) > 20 {
			m.pending = m.pending[1:]
		}
	}
	m.mu.Unlock()
	if session != nil {
		session.Emit(update)
	}
}

func (m *Manager) state() string {
	var b strings.Builder
	b.WriteString("Dispatcher state.")
	if narrating := m.dispatcher.Narrating(); len(narrating) > 0 {
		fmt.Fprintf(&b, " Narrating: %s.", strings.Join(narrating, ", "))
	}
	tickets := m.dispatcher.Tickets()
	if len(tickets) > 0 {
		b.WriteString(" Tickets:")
		start := 0
		if len(tickets) > 8 {
			start = len(tickets) - 8
		}
		for _, ticket := range tickets[start:] {
			fmt.Fprintf(&b, " #%d %s %s (%s);", ticket.ID, ticket.Title, ticket.State, truncate(ticket.Text, 80))
		}
	}
	return b.String()
}

// call routes conversation control to the phone and everything else to the dispatcher.
func (m *Manager) call(ctx context.Context, name, arguments string) (any, error) {
	if name != "conversation" {
		return m.dispatcher.Call(ctx, name, arguments)
	}
	var args toolArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || args.State != "off" {
		return nil, fmt.Errorf("state must be off")
	}
	m.mu.Lock()
	m.phone = args.State
	m.mu.Unlock()
	return "The phone will go " + args.State + " after your next sentence. Say a short goodbye.", nil
}

// Status is what the phone polls: what to show on the orb, Discord replies it must send, and
// whether the backend asked to turn the conversation off. The phone
// request is handed over once.
type Status struct {
	Session   string         `json:"session,omitempty"`
	Phone     string         `json:"phone,omitempty"`
	Narrating []string       `json:"narrating"`
	Tickets   []Ticket       `json:"tickets"`
	Pending   int            `json:"pending"`
	Replies   []DiscordReply `json:"replies"`
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	id := ""
	if m.session != nil {
		id = m.session.ID
	}
	pending := len(m.pending)
	phone := m.phone
	m.phone = ""
	m.mu.Unlock()
	status := Status{Session: id, Phone: phone, Narrating: m.dispatcher.Narrating(), Tickets: m.dispatcher.Tickets(), Pending: pending, Replies: m.discord.Pending()}
	if status.Narrating == nil {
		status.Narrating = []string{}
	}
	if status.Replies == nil {
		status.Replies = []DiscordReply{}
	}
	return status
}

// Handle serves one IPC request from the phone's agency cloud live command.
func (m *Manager) Handle(ctx context.Context, payload string) (string, error) {
	var request struct {
		Op       string           `json:"op"`
		Voice    string           `json:"voice"`
		Accent   string           `json:"accent"`
		SDP      string           `json:"sdp"`
		Text     string           `json:"text"`
		Messages []DiscordMessage `json:"messages"`
		ID       int              `json:"id"`
		Error    string           `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		return "", fmt.Errorf("invalid live request")
	}
	switch request.Op {
	case "start":
		return m.Start(ctx, startRequest{Voice: request.Voice, Accent: request.Accent, SDP: request.SDP})
	case "status":
		data, _ := json.Marshal(m.Status())
		return string(data), nil
	case "said":
		// Words heard on the phone while the session was reconnecting.
		if strings.TrimSpace(request.Text) == "" {
			return "ok", nil
		}
		m.memory.Add("user", request.Text, time.Now())
		m.mu.Lock()
		session := m.session
		m.mu.Unlock()
		if session != nil {
			session.Instruct("Lemon said this while you were reconnecting: \"" + truncate(request.Text, 600) + "\". Respond to it now, then listen.")
		}
		return "ok", nil
	case "discord":
		m.discord.Post(request.Messages)
		return "ok", nil
	case "discord-done":
		m.discord.Done(request.ID, request.Error)
		return "ok", nil
	case "close":
		m.mu.Lock()
		session := m.session
		m.mu.Unlock()
		if session != nil {
			session.Close()
		}
		return "ok", nil
	}
	return "", fmt.Errorf("unknown live op")
}

type wsConn struct{ conn *websocket.Conn }

func (w wsConn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := w.conn.Read(ctx)
	return data, err
}

func (w wsConn) Write(ctx context.Context, event []byte) error {
	return w.conn.Write(ctx, websocket.MessageText, event)
}

func (w wsConn) Close() error {
	return w.conn.Close(websocket.StatusNormalClosure, "")
}

func post(ctx context.Context, client *http.Client, url, key string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("live session request failed")
	}
	defer response.Body.Close()
	reply := make([]byte, 0, 4096)
	buffer := make([]byte, 32*1024)
	for len(reply) < 512*1024 {
		n, err := response.Body.Read(buffer)
		reply = append(reply, buffer[:n]...)
		if err != nil {
			break
		}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(reply, &failure)
		log.Printf("live: session request failed: HTTP %d", response.StatusCode)
		return nil, fmt.Errorf("live session request returned HTTP %d: %s", response.StatusCode, truncate(strings.ReplaceAll(failure.Error.Message, key, "[key]"), 300))
	}
	return reply, nil
}
