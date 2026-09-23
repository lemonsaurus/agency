package live

import (
	"fmt"
	"strings"
	"time"
)

// Mode says how a followed pane reaches the conversation.
type Mode string

const (
	Narrate    Mode = "narrate"
	Background Mode = "background"
)

func ParseMode(value string) Mode {
	if strings.EqualFold(value, string(Background)) {
		return Background
	}
	return Narrate
}

// Update is one message for the voice model: spoken as commentary or kept as quiet context.
type Update struct {
	Spoken  bool
	Content string
}

const (
	// Live accepts 500 tokens per append.
	updateLimit = 1400
	// Narrated progress is rolled up into one line per quiet period.
	quietPeriod = 20 * time.Second
)

// Narrator turns a pane's transcript into a walkthrough: the plan once, failures and answers at
// once, otherwise one rolled-up progress line per quiet period. Background panes report only the finish.
type Narrator struct {
	Session string
	Asked   string
	Mode    Mode
	Done    bool

	seen         map[string]bool
	started      bool
	startedAt    time.Time
	lastSpokenAt time.Time
	spokenPlan   bool
	pendingTools []Turn
	lastFinal    string
	hadFinal     bool
}

func NewNarrator(session, asked string, mode Mode) *Narrator {
	return &Narrator{Session: session, Asked: asked, Mode: mode, seen: map[string]bool{}}
}

func (n *Narrator) Digest(turns []Turn, now time.Time) []Update {
	if n.Done {
		return nil
	}
	start := -1
	for i, turn := range turns {
		if turn.Kind == "lemon" && (n.Asked == "" || strings.TrimSpace(turn.Text) == strings.TrimSpace(n.Asked)) {
			start = i
		}
	}
	if start == -1 {
		return nil
	}
	var fresh []Turn
	for _, turn := range turns[start+1:] {
		// Tool turns are announced once they have a result, so failures are part of the same line.
		if turn.Kind == "tool" && turn.Result == "" && !turn.Error {
			continue
		}
		key := n.key(turn)
		if n.seen[key] {
			continue
		}
		n.seen[key] = true
		fresh = append(fresh, turn)
	}
	var updates []Update
	if !n.started {
		n.started = true
		n.startedAt = now
		n.lastSpokenAt = now
		if n.Mode == Background {
			updates = append(updates, Update{false, fmt.Sprintf("%s is working on this in the background: %s", n.Session, truncate(n.Asked, 200))})
		} else {
			updates = append(updates, Update{true, fmt.Sprintf("%s has started on: %s. Progress comes as it happens.", n.Session, truncate(n.Asked, 200))})
		}
	}
	if n.Mode == Narrate {
		elapsed := fmt.Sprintf("%d min in", int(now.Sub(n.startedAt).Minutes()))
		for _, turn := range fresh {
			switch turn.Kind {
			case "carla":
				if turn.Text != "" {
					updates = append(updates, Update{true, fmt.Sprintf("%s says: %s", n.Session, truncate(turn.Text, updateLimit))})
					n.lastSpokenAt = now
				} else if turn.Thinking != "" && !n.spokenPlan {
					n.spokenPlan = true
					plan := turn.Thinking
					if i := strings.LastIndex(plan, "\n\n"); i >= 0 {
						plan = plan[i+2:]
					}
					updates = append(updates, Update{true, fmt.Sprintf("%s's plan: %s", n.Session, truncate(plan, 300))})
					n.lastSpokenAt = now
				}
			case "tool":
				if turn.Error {
					updates = append(updates, Update{true, fmt.Sprintf("%s, %s failed in %s: %s", elapsed, turn.Name, n.Session, truncate(turn.Result, 200))})
					n.lastSpokenAt = now
				} else {
					n.pendingTools = append(n.pendingTools, turn)
				}
			}
		}
		if len(n.pendingTools) > 0 && now.Sub(n.lastSpokenAt) >= quietPeriod {
			var kinds []string
			seen := map[string]bool{}
			for _, tool := range n.pendingTools {
				if !seen[tool.Name] {
					seen[tool.Name] = true
					kinds = append(kinds, tool.Name)
				}
			}
			latest := n.pendingTools[len(n.pendingTools)-1]
			updates = append(updates, Update{true, strings.TrimSpace(fmt.Sprintf("%s, %s is still going: %d steps since last time (%s), now on %s %s",
				elapsed, n.Session, len(n.pendingTools), strings.Join(kinds, ", "), latest.Name, truncate(latest.Summary, 80)))})
			n.pendingTools = nil
			n.lastSpokenAt = now
		}
	}
	last := turns[len(turns)-1]
	final, isFinal := "", false
	if last.Kind == "carla" && last.Text != "" {
		final, isFinal = last.Text, true
	}
	if isFinal && n.hadFinal && final == n.lastFinal {
		n.Done = true
		if n.Mode == Background {
			updates = append(updates, Update{true, fmt.Sprintf("%s finished the background task. %s", n.Session, truncate(final, updateLimit-80))})
		} else {
			updates = append(updates, Update{false, fmt.Sprintf("%s is idle now; that was its final answer.", n.Session)})
		}
	}
	n.lastFinal, n.hadFinal = final, isFinal
	return updates
}

func (n *Narrator) key(turn Turn) string {
	switch turn.Kind {
	case "lemon":
		return fmt.Sprintf("lemon:%d:%s", turn.At, turn.Text)
	case "carla":
		return "carla:" + turn.Text + "\x00" + turn.Thinking
	case "tool":
		return "tool:" + turn.Name + ":" + turn.Summary
	default:
		return "skill:" + turn.Name
	}
}
