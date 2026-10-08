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
// sideband, and keeps the dispatcher, Discord relay, reminders and memory alive between sessions.
type Manager struct {
	box        Box
	key        string
	persona    func() (string, error)
	prompts    string
	memory     *Memory
	recall     *Recall
	dispatcher *Dispatcher
	discord    *Discord
	reminders  *Reminders
	rosa       *Rosa
	client     *http.Client
	API        string
	// Codex signs the voice backend's requests with Lemon's ChatGPT login.
	Codex *CodexToken

	mu      sync.Mutex
	session *Session
	pending []Update
	said    []string
	phone   string
}

func NewManager(box Box, key string, persona func() (string, error), promptDir, memoryPath string) *Manager {
	m := &Manager{box: box, key: key, persona: persona, prompts: promptDir, memory: OpenMemory(memoryPath), recall: &Recall{}, client: &http.Client{Timeout: 120 * time.Second}, API: "https://api.openai.com"}
	m.discord = NewDiscord(m.Emit)
	remindersPath := ""
	if memoryPath != "" {
		remindersPath = filepath.Join(filepath.Dir(memoryPath), "reminders.json")
	}
	m.reminders = OpenReminders(remindersPath, m.Emit)
	m.dispatcher = NewDispatcher(box, m.discord, m.Emit)
	m.dispatcher.Diary = filepath.Join(filepath.Dir(memoryPath), "activity.jsonl")
	m.rosa = NewRosa(filepath.Join(promptDir, "rosa"), filepath.Join(filepath.Dir(memoryPath), "rosa"))
	m.rosa.Backend = func() *Backend {
		_, backend, _, err := m.rosa.Instructions()
		if err != nil {
			return nil
		}
		return m.backend(backend, RosaSchema, m.rosa.Call)
	}
	m.rosa.Restore()
	return m
}

type startRequest struct {
	Agent  string `json:"agent"`
	Voice  string `json:"voice"`
	Accent string `json:"accent"`
	Zone   string `json:"zone"`
	SDP    string `json:"sdp"`
	// Rolls is a phone that can hand a long Rosa call to a fresh session.
	Rolls bool `json:"rolls"`
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
	m.reminders.SetZone(request.Zone)
	zone := m.reminders.Zone()
	var live, backend, voice string
	var seed []map[string]any
	var err error
	switch request.Agent {
	case "", "carla":
		live, backend, err = m.instructions(request.Accent)
		voice, seed = request.Voice, m.memory.Seed(time.Now().In(zone))
	case "rosa":
		live, backend, voice, err = m.rosa.Instructions()
		seed = m.rosa.Seed(time.Now().In(zone))
	default:
		err = fmt.Errorf("unknown agent %q", request.Agent)
	}
	if err != nil {
		return "", err
	}
	config := map[string]any{
		"model":        "gpt-live-1",
		"instructions": live,
		"audio":        map[string]any{"output": map[string]any{"voice": voice}},
		"delegation":   map[string]any{"type": "client"},
	}
	if len(seed) > 0 {
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
	conn, err := m.dial(answer.Session.ID)
	if err != nil {
		return "", err
	}
	if request.Agent == "rosa" {
		var roll func()
		if request.Rolls {
			roll = func() {
				m.mu.Lock()
				m.phone = "roll"
				m.mu.Unlock()
			}
		}
		m.adopt(m.rosa.Session(answer.Session.ID, conn, m.backend(backend, RosaSchema, m.rosa.Call), zone, m.turnOff, roll))
		return string(reply), nil
	}
	session := NewSession(answer.Session.ID, conn, m.memory, m.recall, m.backend(backend, Schema, m.call), m.state)
	session.Zone = zone
	m.mu.Lock()
	pending := m.pending
	m.pending = nil
	m.mu.Unlock()
	m.adopt(session)
	for _, update := range pending {
		session.Emit(update)
	}
	return string(reply), nil
}

// SetPush lets reminders wake the phone through FCM.
func (m *Manager) SetPush(p *Pusher) { m.reminders.Push = p }

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

// dial attaches the sideband to a Live session.
func (m *Manager) dial(id string) (Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	url := strings.Replace(m.API, "http", "ws", 1) + "/v1/live/sessions/" + id + "/attach"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + m.key}}, HTTPClient: m.client})
	if err != nil {
		return nil, fmt.Errorf("sideband attach failed: %v", err)
	}
	conn.SetReadLimit(8 << 20)
	return wsConn{conn}, nil
}

func (m *Manager) backend(instructions string, schema json.RawMessage, tools func(ctx context.Context, name, arguments string) (any, error)) *Backend {
	return &Backend{Client: m.client, URL: CodexURL, Auth: m.Codex.Get, Model: "gpt-6.1-sol", Instructions: instructions, Schema: schema, Tools: tools}
}

// adopt makes session the phone's current call, closing the one before it.
func (m *Manager) adopt(session *Session) {
	m.mu.Lock()
	previous := m.session
	m.session = session
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
}

// Emit routes an update to the live session, or holds spoken ones for the next session.
func (m *Manager) Emit(update Update) {
	m.recall.Note(update.Content, time.Now())
	m.mu.Lock()
	session := m.session
	if session != nil && session.Agent != "carla" {
		session = nil
	}
	if session == nil && update.Spoken {
		update.Content = "(From " + time.Now().In(m.reminders.Zone()).Format("15:04") + ", while the call was off) " + update.Content
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

// call routes conversation control and reminders to the phone and everything else to the dispatcher.
func (m *Manager) call(ctx context.Context, name, arguments string) (any, error) {
	if name != "conversation" && name != "remind" {
		return m.dispatcher.Call(ctx, name, arguments)
	}
	var args toolArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments")
	}
	if name == "remind" {
		return m.reminders.Add(args.At, args.Text, time.Now().In(m.reminders.Zone()))
	}
	if args.State != "off" {
		return nil, fmt.Errorf("state must be off")
	}
	return m.turnOff(), nil
}

// turnOff asks the phone to end the call once the goodbye is said.
func (m *Manager) turnOff() string {
	m.mu.Lock()
	m.phone = "off"
	m.mu.Unlock()
	return "The phone will go off after your next sentence. Say a short goodbye."
}

// Status is what the phone polls: what to show on the orb, Discord replies it must send, reminders
// it must schedule, and
// whether the backend asked to turn the conversation off. The phone
// request is handed over once.
type Status struct {
	Session   string         `json:"session,omitempty"`
	Phone     string         `json:"phone,omitempty"`
	Rolling   bool           `json:"rolling,omitempty"`
	Narrating []string       `json:"narrating"`
	Tickets   []Ticket       `json:"tickets"`
	Pending   int            `json:"pending"`
	Replies   []DiscordReply `json:"replies"`
	Reminders []Reminder     `json:"reminders"`
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
	status := Status{Session: id, Phone: phone, Rolling: m.rosa.Rolling(), Narrating: m.dispatcher.Narrating(), Tickets: m.dispatcher.Tickets(), Pending: pending, Replies: m.discord.Pending(), Reminders: m.reminders.Pending(time.Now())}
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
		Agent    string           `json:"agent"`
		Voice    string           `json:"voice"`
		Accent   string           `json:"accent"`
		Zone     string           `json:"zone"`
		Token    string           `json:"token"`
		When     string           `json:"when"`
		SDP      string           `json:"sdp"`
		Rolls    bool             `json:"rolls"`
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
		return m.Start(ctx, startRequest{Agent: request.Agent, Voice: request.Voice, Accent: request.Accent, Zone: request.Zone, SDP: request.SDP, Rolls: request.Rolls})
	case "status":
		data, _ := json.Marshal(m.Status())
		return string(data), nil
	case "said":
		// Words heard on the phone while the session was reconnecting.
		if strings.TrimSpace(request.Text) == "" {
			return "ok", nil
		}
		m.mu.Lock()
		session := m.session
		m.mu.Unlock()
		memory := m.memory
		if session != nil {
			memory = session.memory
		}
		memory.Add("user", request.Text, time.Now())
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
	case "reminder-done":
		m.reminders.Done(request.ID, request.Error)
		return "ok", nil
	case "reminders":
		data, _ := json.Marshal(m.reminders.Upcoming(time.Now()))
		return string(data), nil
	case "push-token":
		// The phone reports in on every app start: its push address and the zone it is in.
		m.reminders.SetToken(request.Token)
		m.reminders.SetZone(request.Zone)
		return "ok", nil
	case "remind":
		now := time.Now().In(m.reminders.Zone())
		if request.When == "" && request.Text == "" {
			return "It is " + clock(now) + ". Usage: agency remind <+20m|YYYY-MM-DDTHH:MM> <text>", nil
		}
		result, err := m.reminders.Add(request.When, request.Text, now)
		if err != nil {
			return "", err
		}
		return result["result"].(string), nil
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
