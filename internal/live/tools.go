package live

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Dispatcher runs tool calls for the backend model. Asks become tickets that start the work and
// return at once; a Narrator per pane carries the progress back into the conversation.
type Dispatcher struct {
	box     Box
	discord *Discord
	emit    func(Update)

	mu        sync.Mutex
	narrators map[string]*Narrator
	owned     map[string]string // project path → pane the phone opened there
	tickets   []*Ticket
	nextID    int
	polling   bool
}

// Ticket is one queued instruction to a pane.
type Ticket struct {
	ID     int       `json:"id"`
	Pane   string    `json:"pane"`
	Title  string    `json:"title"`
	Text   string    `json:"text"`
	Mode   Mode      `json:"mode"`
	State  string    `json:"state"` // queued, started, failed, done
	Error  string    `json:"error,omitempty"`
	At     time.Time `json:"at"`
	paneID string
}

func NewDispatcher(box Box, discord *Discord, emit func(Update)) *Dispatcher {
	return &Dispatcher{box: box, discord: discord, emit: emit, narrators: map[string]*Narrator{}, owned: map[string]string{}}
}

// Schema is the function list the backend model sees.
var Schema = json.RawMessage(`[
{"type":"function","name":"list_panes","description":"List open sessions with their task names and status, plus the projects on the box where a session can be opened.","parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}},
{"type":"function","name":"ask_project","description":"Ask a question or give an instruction inside a project. Reuses the phone's own session in that project or opens one. Queued and returned at once; the session's progress and answer arrive in the conversation on their own. Use this by default for anything project-related; no permission needed. Never automatically retry.","parameters":{"type":"object","properties":{"project_path":{"type":"string"},"text":{"type":"string"},"mode":{"type":"string","enum":["narrate","background"],"description":"narrate: Lemon is focused on this and wants to hear progress. background: he asked to set it up and move on; only the finish is announced."}},"required":["project_path","text","mode"],"additionalProperties":false}},
{"type":"function","name":"ask_pane","description":"Send Lemon's instruction to a specific existing session he named. Queued and returned at once; progress and answer arrive in the conversation on their own. Never automatically retry.","parameters":{"type":"object","properties":{"pane_id":{"type":"string"},"text":{"type":"string"},"mode":{"type":"string","enum":["narrate","background"]}},"required":["pane_id","text","mode"],"additionalProperties":false}},
{"type":"function","name":"check_pane","description":"Check in on a session: whether it is working or idle, what it is doing now, and its last answer.","parameters":{"type":"object","properties":{"pane_id":{"type":"string"}},"required":["pane_id"],"additionalProperties":false}},
{"type":"function","name":"follow_pane","description":"Switch how a running session reaches the conversation: narrate progress, or background until it finishes.","parameters":{"type":"object","properties":{"pane_id":{"type":"string"},"mode":{"type":"string","enum":["narrate","background"]}},"required":["pane_id","mode"],"additionalProperties":false}},
{"type":"function","name":"read_pane","description":"Read what is currently on a session's screen without sending anything.","parameters":{"type":"object","properties":{"pane_id":{"type":"string"}},"required":["pane_id"],"additionalProperties":false}},
{"type":"function","name":"close_pane","description":"Close a session when Lemon asks for it. Its work stops.","parameters":{"type":"object","properties":{"pane_id":{"type":"string"}},"required":["pane_id"],"additionalProperties":false}},
{"type":"function","name":"spawn_pane","description":"Open a named session for a project when Lemon asks for one to keep.","parameters":{"type":"object","properties":{"project_path":{"type":"string"},"label":{"type":"string"}},"required":["project_path","label"],"additionalProperties":false}},
{"type":"function","name":"tickets","description":"What the dispatcher is doing: queued, running and finished instructions to sessions.","parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}},
{"type":"function","name":"discord_recent","description":"Recent Discord messages the phone was notified about: id, sender, place, text, and whether a reply can still be sent.","parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}},
{"type":"function","name":"conversation","description":"Change the phone's conversation state when Lemon asks to stop, pause, go quiet, or turn off. doze: close the voice stream but keep listening on the phone and wake for news or speech. off: stop listening entirely. Takes effect after your next sentence, so say a short goodbye.","parameters":{"type":"object","properties":{"state":{"type":"string","enum":["doze","off"]}},"required":["state"],"additionalProperties":false}},
{"type":"function","name":"discord_reply","description":"Send Lemon's dictated reply to a Discord message by id, through Discord's own notification on the phone. Only after Lemon has said the exact wording. Never automatically retry.","parameters":{"type":"object","properties":{"id":{"type":"integer"},"text":{"type":"string"}},"required":["id","text"],"additionalProperties":false}}
]`)

type toolArgs struct {
	PaneID      string `json:"pane_id"`
	ProjectPath string `json:"project_path"`
	Text        string `json:"text"`
	Mode        string `json:"mode"`
	Label       string `json:"label"`
	ID          int    `json:"id"`
	State       string `json:"state"`
}

// Call runs one tool and returns its JSON result. Every call returns within a few seconds.
func (d *Dispatcher) Call(ctx context.Context, name, arguments string) (any, error) {
	var args toolArgs
	if arguments != "" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return nil, fmt.Errorf("invalid arguments")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	switch name {
	case "list_panes":
		panes, err := d.box.Panes(ctx)
		if err != nil {
			return nil, err
		}
		projects, err := d.box.Projects()
		if err != nil {
			return nil, err
		}
		var listed []map[string]any
		for _, pane := range panes {
			status := "ready"
			if !pane.Bridge {
				status = "starting; instructions wait until it is ready"
			}
			listed = append(listed, map[string]any{"id": pane.ID, "name": pane.Title(), "project": pane.Dir, "status": status})
		}
		return map[string]any{"panes": listed, "projects": projects}, nil
	case "ask_pane":
		pane, err := d.pane(ctx, args.PaneID)
		if err != nil {
			return nil, err
		}
		return d.enqueue(pane, args.Text, ParseMode(args.Mode)), nil
	case "ask_project":
		project, err := d.project(args.ProjectPath)
		if err != nil {
			return nil, err
		}
		// Reuse the pane the phone opened in this project; otherwise spawn one unlabeled and let Pi
		// name it from the first prompt. deliver finds it as the bridged pane in that directory
		// that did not exist before the spawn.
		panes, err := d.box.Panes(ctx)
		if err != nil {
			return nil, err
		}
		d.mu.Lock()
		ownedID := d.owned[project.Path]
		d.mu.Unlock()
		var pane *Pane
		existing := map[string]bool{}
		for i := range panes {
			existing[panes[i].ID] = true
			if panes[i].ID == ownedID {
				pane = &panes[i]
			}
		}
		if pane == nil {
			if err := d.box.Spawn(ctx, project.Path, ""); err != nil {
				return nil, fmt.Errorf("could not start a session in %s: %v", project.Name, err)
			}
			pane = &Pane{Dir: project.Path, before: existing}
		}
		return d.enqueue(*pane, args.Text, ParseMode(args.Mode)), nil
	case "check_pane":
		pane, err := d.pane(ctx, args.PaneID)
		if err != nil {
			return nil, err
		}
		raw, err := d.box.Transcript(ctx, pane.ID, 12)
		if err != nil {
			return nil, err
		}
		turns := ParseTranscript(raw)
		result := map[string]any{"pane": pane.Title(), "state": "working"}
		if len(turns) > 0 {
			last := turns[len(turns)-1]
			if last.Kind == "carla" && last.Text != "" {
				result["state"] = "idle"
			} else if last.Kind == "lemon" {
				result["current"] = "just received: " + truncate(last.Text, 200)
			} else {
				for i := len(turns) - 1; i >= 0; i-- {
					if turns[i].Kind == "tool" {
						result["current"] = strings.TrimSpace(turns[i].Name + " " + truncate(turns[i].Summary, 120))
						break
					}
				}
			}
			for i := len(turns) - 1; i >= 0; i-- {
				if turns[i].Kind == "carla" && turns[i].Text != "" {
					result["last_answer"] = truncate(turns[i].Text, updateLimit)
					break
				}
			}
		}
		return result, nil
	case "follow_pane":
		pane, err := d.pane(ctx, args.PaneID)
		if err != nil {
			return nil, err
		}
		mode := ParseMode(args.Mode)
		d.follow(pane, "", mode)
		if mode == Narrate {
			return map[string]any{"result": "Narrating " + pane.Title() + " from here on."}, nil
		}
		return map[string]any{"result": pane.Title() + " continues quietly; you will be told when it finishes."}, nil
	case "read_pane":
		pane, err := d.pane(ctx, args.PaneID)
		if err != nil {
			return nil, err
		}
		screen, err := d.box.Screen(ctx, pane.ID, 120)
		if err != nil {
			return nil, err
		}
		if len(screen) > 6000 {
			screen = screen[len(screen)-6000:]
		}
		return map[string]any{"screen": screen}, nil
	case "close_pane":
		pane, err := d.pane(ctx, args.PaneID)
		if err != nil {
			return nil, err
		}
		if err := d.box.Kill(ctx, pane.ID); err != nil {
			return nil, err
		}
		d.mu.Lock()
		delete(d.narrators, pane.ID)
		d.mu.Unlock()
		return map[string]any{"result": "Closed " + pane.Title() + "."}, nil
	case "spawn_pane":
		project, err := d.project(args.ProjectPath)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(args.Label) == "" || len(args.Label) > 100 {
			return nil, fmt.Errorf("use a task label of 1 to 100 characters")
		}
		if err := d.box.Spawn(ctx, project.Path, args.Label); err != nil {
			return nil, err
		}
		return map[string]any{"result": args.Label + " is starting in " + project.Name + "."}, nil
	case "tickets":
		return map[string]any{"tickets": d.Tickets()}, nil
	case "discord_recent":
		return map[string]any{"messages": d.discord.Recent()}, nil
	case "discord_reply":
		return d.discord.Reply(args.ID, args.Text)
	}
	return nil, fmt.Errorf("unknown tool: %s", name)
}

func (d *Dispatcher) pane(ctx context.Context, id string) (Pane, error) {
	panes, err := d.box.Panes(ctx)
	if err != nil {
		return Pane{}, err
	}
	for _, pane := range panes {
		if pane.ID == id {
			return pane, nil
		}
	}
	return Pane{}, fmt.Errorf("that session is no longer open; list sessions again")
}

func (d *Dispatcher) project(path string) (Project, error) {
	projects, err := d.box.Projects()
	if err != nil {
		return Project{}, err
	}
	for _, project := range projects {
		if project.Path == path {
			return project, nil
		}
	}
	return Project{}, fmt.Errorf("choose a project path returned by list_panes")
}

// enqueue records the ticket and starts delivering it; the caller gets a one-line status now.
func (d *Dispatcher) enqueue(pane Pane, text string, mode Mode) map[string]any {
	if strings.TrimSpace(text) == "" {
		return map[string]any{"error": "nothing to send"}
	}
	d.mu.Lock()
	d.nextID++
	ticket := &Ticket{ID: d.nextID, Pane: pane.ID, Title: pane.Title(), Text: text, Mode: mode, State: "queued", At: time.Now(), paneID: pane.ID}
	d.tickets = append(d.tickets, ticket)
	if len(d.tickets) > 50 {
		d.tickets = d.tickets[1:]
	}
	d.mu.Unlock()
	go d.deliver(ticket, pane)
	status := pane.Title() + " is queued; it starts as soon as the session is ready and progress arrives on its own. Do not wait or poll."
	if mode == Background {
		status = pane.Title() + " is queued in the background; you will be told when it finishes."
	}
	return map[string]any{"pane": pane.Title(), "ticket": ticket.ID, "status": status}
}

// deliver waits for the pane's bridge, sends the prompt detached, then follows the pane.
func (d *Dispatcher) deliver(ticket *Ticket, pane Pane) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	ready := pane
	if !pane.Bridge || pane.ID == "" {
		found := false
		for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline) && !found; time.Sleep(1500 * time.Millisecond) {
			panes, err := d.box.Panes(ctx)
			if err != nil {
				continue
			}
			for _, candidate := range panes {
				if candidate.Bridge && ((pane.ID != "" && candidate.ID == pane.ID) || (pane.ID == "" && candidate.Dir == pane.Dir && !pane.before[candidate.ID])) {
					ready, found = candidate, true
					break
				}
			}
		}
		if !found {
			d.fail(ticket, "the session never became ready")
			return
		}
	}
	d.mu.Lock()
	ticket.paneID = ready.ID
	if pane.ID == "" {
		d.owned[ready.Dir] = ready.ID
	}
	d.mu.Unlock()
	if err := d.box.Send(ctx, ready.ID, ticket.Text); err != nil {
		d.fail(ticket, err.Error())
		return
	}
	d.mu.Lock()
	ticket.State = "started"
	d.mu.Unlock()
	d.follow(ready, ticket.Text, ticket.Mode)
}

func (d *Dispatcher) fail(ticket *Ticket, reason string) {
	d.mu.Lock()
	ticket.State, ticket.Error = "failed", reason
	d.mu.Unlock()
	d.emit(Update{true, fmt.Sprintf("Could not hand the instruction to %s: %s.", ticket.Title, reason)})
}

func (d *Dispatcher) follow(pane Pane, asked string, mode Mode) {
	d.mu.Lock()
	if existing, ok := d.narrators[pane.ID]; ok && asked == "" {
		existing.Mode = mode
	} else {
		d.narrators[pane.ID] = NewNarrator(pane.Title(), asked, mode)
	}
	start := !d.polling
	d.polling = true
	d.mu.Unlock()
	if start {
		go d.narrate()
	}
}

func (d *Dispatcher) narrate() {
	for {
		d.mu.Lock()
		current := make(map[string]*Narrator, len(d.narrators))
		for id, narrator := range d.narrators {
			current[id] = narrator
		}
		if len(current) == 0 {
			d.polling = false
			d.mu.Unlock()
			return
		}
		d.mu.Unlock()
		for id, narrator := range current {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			raw, err := d.box.Transcript(ctx, id, 40)
			cancel()
			if err != nil {
				continue
			}
			for _, update := range narrator.Digest(ParseTranscript(raw), time.Now()) {
				d.emit(update)
			}
			if narrator.Done {
				d.mu.Lock()
				delete(d.narrators, id)
				for _, ticket := range d.tickets {
					if ticket.paneID == id && ticket.State == "started" {
						ticket.State = "done"
					}
				}
				d.mu.Unlock()
			}
		}
		time.Sleep(2500 * time.Millisecond)
	}
}

// Narrating reports the panes currently being narrated aloud, for the phone's orb.
func (d *Dispatcher) Narrating() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var titles []string
	for _, narrator := range d.narrators {
		if narrator.Mode == Narrate && !narrator.Done {
			titles = append(titles, narrator.Session)
		}
	}
	return titles
}

func (d *Dispatcher) Tickets() []Ticket {
	d.mu.Lock()
	defer d.mu.Unlock()
	listed := make([]Ticket, 0, len(d.tickets))
	for _, ticket := range d.tickets {
		listed = append(listed, *ticket)
	}
	return listed
}
