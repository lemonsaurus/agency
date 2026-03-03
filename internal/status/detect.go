package status

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	StatusRunning = "running"
	StatusWaiting = "waiting"
	StatusIdle    = "idle"
	StatusDone    = "done"
)

// PaneCapture provides pane content for status detection.
type PaneCapture interface {
	CapturePaneContent(ctx context.Context, paneID string, lines int) (string, error)
}

// StatusCallback is called when a pane's status changes.
type StatusCallback func(paneID, agentType, status string)

// Detect determines agent status from captured pane content.
func Detect(agentType, content string) string {
	if content == "" {
		return StatusIdle
	}

	// Look at the last few lines for status indicators.
	lines := strings.Split(content, "\n")
	tail := lastN(lines, 15)
	joined := strings.Join(tail, "\n")

	switch agentType {
	case "claude":
		return detectClaude(joined)
	case "codex":
		return detectCodex(joined)
	case "gemini":
		return detectGemini(joined)
	default:
		return detectGeneric(joined)
	}
}

func detectClaude(content string) string {
	// Claude shows ❯ when waiting for input.
	if strings.Contains(content, "❯") {
		return StatusWaiting
	}
	// Tool use indicators.
	if strings.Contains(content, "Read(") || strings.Contains(content, "Edit(") ||
		strings.Contains(content, "Write(") || strings.Contains(content, "Bash(") ||
		strings.Contains(content, "Search(") || strings.Contains(content, "Grep(") {
		return StatusRunning
	}
	if strings.Contains(content, "Task(") || strings.Contains(content, "Agent(") {
		return StatusRunning
	}
	return StatusIdle
}

func detectCodex(content string) string {
	if strings.Contains(content, "> ") || strings.Contains(content, "❯") {
		return StatusWaiting
	}
	if strings.Contains(content, "Thinking") || strings.Contains(content, "Running") {
		return StatusRunning
	}
	return StatusIdle
}

func detectGemini(content string) string {
	if strings.Contains(content, ">>> ") || strings.Contains(content, "❯") {
		return StatusWaiting
	}
	if strings.Contains(content, "Generating") || strings.Contains(content, "Thinking") {
		return StatusRunning
	}
	return StatusIdle
}

func detectGeneric(content string) string {
	// Generic: look for common prompt patterns.
	if strings.Contains(content, "$ ") || strings.Contains(content, "❯") || strings.Contains(content, "> ") {
		return StatusWaiting
	}
	return StatusIdle
}

func lastN(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// PaneStatus holds what the poller tracks per pane.
type PaneStatus struct {
	PaneID    string
	AgentType string
	Status    string
}

// Poller periodically captures pane content and detects agent status.
type Poller struct {
	capture  PaneCapture
	callback StatusCallback
	interval time.Duration

	mu    sync.Mutex
	panes map[string]PaneStatus // keyed by pane ID
}

// NewPoller creates a status poller.
func NewPoller(capture PaneCapture, callback StatusCallback) *Poller {
	return &Poller{
		capture:  capture,
		callback: callback,
		interval: 2 * time.Second,
		panes:    make(map[string]PaneStatus),
	}
}

// Track registers a pane for status polling.
func (p *Poller) Track(paneID, agentType string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.panes[paneID] = PaneStatus{
		PaneID:    paneID,
		AgentType: agentType,
		Status:    StatusIdle,
	}
}

// Untrack removes a pane from polling.
func (p *Poller) Untrack(paneID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.panes, paneID)
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	p.mu.Lock()
	panes := make([]PaneStatus, 0, len(p.panes))
	for _, ps := range p.panes {
		panes = append(panes, ps)
	}
	p.mu.Unlock()

	for _, ps := range panes {
		content, err := p.capture.CapturePaneContent(ctx, ps.PaneID, 30)
		if err != nil {
			log.Printf("status: capture %s: %v", ps.PaneID, err)
			continue
		}

		newStatus := Detect(ps.AgentType, content)

		p.mu.Lock()
		current, ok := p.panes[ps.PaneID]
		if ok && current.Status != newStatus {
			current.Status = newStatus
			p.panes[ps.PaneID] = current
			p.mu.Unlock()
			if p.callback != nil {
				p.callback(ps.PaneID, ps.AgentType, newStatus)
			}
		} else {
			p.mu.Unlock()
		}
	}
}
