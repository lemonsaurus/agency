package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/lemonsaurus/agency/internal/budget"
	"github.com/lemonsaurus/agency/internal/control"
)

// QueuedPrefix starts a spawn reply that queued instead of returning a pane id.
const QueuedPrefix = "queued "

// spawnRequest is a queued spawn, enough to replay it after a daemon restart.
type spawnRequest struct {
	Requester control.Requester `json:"requester"`
	Role      control.Role      `json:"role"`
	Window    string            `json:"window,omitempty"`
	Agent     string            `json:"agent,omitempty"`
	Command   string            `json:"command,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Label     string            `json:"label,omitempty"`
	Prompt    string            `json:"prompt,omitempty"`
}

// Admission puts every spawn through the budget. Spawns that fit run at
// once; the rest queue, and the requester hears the pane id when it starts.
type Admission struct {
	Handler
	Budget *budget.Budget
	// QueuePath keeps queued spawns across a daemon restart.
	QueuePath string
	ctx       context.Context
}

// NewAdmission wraps handler so its spawns go through b.
func NewAdmission(ctx context.Context, handler Handler, b *budget.Budget, stateDir string) *Admission {
	return &Admission{Handler: handler, Budget: b, QueuePath: filepath.Join(stateDir, "queue.json"), ctx: ctx}
}

func (a *Admission) SpawnAgent(ctx context.Context, requester control.Requester, role control.Role, name, dir, label, prompt string) (string, error) {
	return a.admit(spawnRequest{Requester: requester, Role: role, Agent: name, Dir: dir, Label: label, Prompt: prompt})
}

func (a *Admission) SpawnAgentWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, name, dir, label, prompt string) (string, error) {
	return a.admit(spawnRequest{Requester: requester, Role: role, Window: windowName, Agent: name, Dir: dir, Label: label, Prompt: prompt})
}

func (a *Admission) SpawnCommand(ctx context.Context, requester control.Requester, role control.Role, command, dir, label string) (string, error) {
	return a.admit(spawnRequest{Requester: requester, Role: role, Command: command, Dir: dir, Label: label})
}

func (a *Admission) SpawnCommandWindow(ctx context.Context, requester control.Requester, role control.Role, windowName, command, dir, label string) (string, error) {
	return a.admit(spawnRequest{Requester: requester, Role: role, Window: windowName, Command: command, Dir: dir, Label: label})
}

// start runs a spawn on the wrapped handler.
func (a *Admission) start(r spawnRequest) func() (string, error) {
	return func() (string, error) {
		switch {
		case r.Agent != "" && r.Window != "":
			return a.Handler.SpawnAgentWindow(a.ctx, r.Requester, r.Role, r.Window, r.Agent, r.Dir, r.Label, r.Prompt)
		case r.Agent != "":
			return a.Handler.SpawnAgent(a.ctx, r.Requester, r.Role, r.Agent, r.Dir, r.Label, r.Prompt)
		case r.Window != "":
			return a.Handler.SpawnCommandWindow(a.ctx, r.Requester, r.Role, r.Window, r.Command, r.Dir, r.Label)
		}
		return a.Handler.SpawnCommand(a.ctx, r.Requester, r.Role, r.Command, r.Dir, r.Label)
	}
}

// SpawnClass names the kind of pane a spawn makes, matching PaneClass.
func SpawnClass(role control.Role, agentOrCommand string) string {
	fields := strings.Fields(agentOrCommand)
	base := "shell"
	if len(fields) > 0 {
		base = filepath.Base(fields[0])
	}
	return string(role) + "/" + base
}

func (a *Admission) admit(r spawnRequest) (string, error) {
	class := SpawnClass(r.Role, r.Agent+r.Command)
	payload, _ := json.Marshal(r)
	start := a.start(r)
	d := a.Budget.Spawn(budget.Ticket{
		Class:   class,
		Label:   r.Label,
		Role:    r.Requester.Role,
		Human:   r.Requester.Human,
		Pane:    r.Requester.PaneID,
		Cost:    a.Budget.SpawnCost(class),
		Payload: string(payload),
	}, start)
	if !d.Admitted {
		a.saveQueue()
		return QueuedReply(d), nil
	}
	if d.Warning != "" {
		log.Printf("budget: %s spawn %q admitted %s", class, r.Label, d.Warning)
	}
	paneID, err := start()
	a.Budget.Spawned(d.Ticket, paneID)
	return paneID, err
}

// QueuedReply tells a spawner its spawn waits, where, and why.
func QueuedReply(d budget.Decision) string {
	return fmt.Sprintf("%s%s: position %d of %d, %s. It starts on its own when the box has room and you get a message with the pane id. Cancel: agency budget cancel %s",
		QueuedPrefix, d.Ticket, d.Position, d.Waiting, d.Reason, d.Ticket)
}

// Started tells a queued spawn's requester how it went, and keeps the saved
// queue current. A requester whose Pi has no bridge gets the text typed in,
// as `agency send` does.
func (a *Admission) Started(s budget.Started, notify func(paneID, text string) bool) {
	a.saveQueue()
	label := s.Ticket.Label
	if label == "" {
		label = s.Ticket.Class
	}
	text := fmt.Sprintf("[agency] queued spawn %s %q started as %s.", s.Ticket.ID, label, s.PaneID)
	if s.Err != nil {
		text = fmt.Sprintf("[agency] queued spawn %s %q failed to start: %v", s.Ticket.ID, label, s.Err)
	}
	if s.Ticket.Pane == "" {
		log.Print(text)
		return
	}
	if notify(s.Ticket.Pane, text) {
		return
	}
	if err := a.Handler.SendText(a.ctx, control.Requester{Human: true}, s.Ticket.Pane, text, true); err != nil {
		log.Printf("%s Telling %s failed: %v", text, s.Ticket.Pane, err)
	}
}

func (a *Admission) saveQueue() {
	data, err := json.MarshalIndent(a.Budget.Queued(), "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(a.QueuePath), 0o755)
	tmp := a.QueuePath + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, a.QueuePath)
	}
}

// RestoreQueue queues the spawns saved before a restart.
func (a *Admission) RestoreQueue() {
	data, err := os.ReadFile(a.QueuePath)
	if err != nil {
		return
	}
	var tickets []budget.Ticket
	if json.Unmarshal(data, &tickets) != nil {
		return
	}
	a.Budget.Restore(tickets, func(t budget.Ticket) func() (string, error) {
		var r spawnRequest
		if json.Unmarshal([]byte(t.Payload), &r) != nil {
			return nil
		}
		return a.start(r)
	})
}

// Notify hands text to a pane's Pi bridge. It is false when the pane has none.
func (s *Server) Notify(paneID, text string) bool {
	return s.deliver(paneID, text)
}

// JobRequest is what `agency job` sends: empty Ticket first, then its ticket
// on every poll and heartbeat.
type JobRequest struct {
	Ticket string      `json:"ticket,omitempty"`
	Class  string      `json:"class,omitempty"`
	Label  string      `json:"label,omitempty"`
	Cost   budget.Cost `json:"cost,omitempty"`
	PID    int         `json:"pid,omitempty"`
}

// budgetDispatch answers budget, budget-cancel, job and job-done. ok is false
// for every other line.
func (s *Server) budgetDispatch(line string, pid int) (reply string, ok bool, err error) {
	cmd, arg, _ := strings.Cut(line, ":")
	switch cmd {
	case "budget":
		data, err := json.Marshal(s.Budget.Snapshot())
		return string(data), true, err
	case "budget-cancel":
		requester, err := s.requester(pid)
		if err != nil {
			return "", true, err
		}
		for _, t := range s.Budget.Snapshot().Queue {
			if t.ID != arg {
				continue
			}
			if !requester.Human && requester.Role != control.RoleController && requester.PaneID != t.Pane {
				return "", true, fmt.Errorf("%s belongs to %s", arg, t.Pane)
			}
			s.Budget.Cancel(arg)
			if s.Admission != nil {
				s.Admission.saveQueue()
			}
			return "cancelled " + arg, true, nil
		}
		return "", true, fmt.Errorf("nothing queued as %s", arg)
	case "job":
		var request JobRequest
		if err := json.Unmarshal([]byte(arg), &request); err != nil {
			return "", true, fmt.Errorf("invalid job payload: %w", err)
		}
		requester, _ := s.requester(pid)
		d := s.Budget.Job(budget.Ticket{
			ID:    request.Ticket,
			Class: request.Class,
			Label: request.Label,
			Role:  requester.Role,
			Human: requester.Human,
			Pane:  requester.PaneID,
			PID:   request.PID,
			Cost:  request.Cost,
		})
		data, err := json.Marshal(d)
		return string(data), true, err
	case "job-done":
		s.Budget.JobDone(arg)
		return "ok", true, nil
	}
	return "", false, nil
}
