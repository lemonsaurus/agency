package live

import "strings"

// Glance is a session at a glance. It is working until its last turn is a spoken answer.
type Glance struct {
	State  string `json:"state"`            // working, idle
	Doing  string `json:"doing,omitempty"`  // current tool step, or the prompt it just received
	Answer string `json:"answer,omitempty"` // last answer, up to the limit
	At     int64  `json:"at,omitempty"`     // last activity, Unix milliseconds
}

func GlanceAt(turns []Turn, limit int) Glance {
	if len(turns) == 0 {
		return Glance{State: "idle"}
	}
	last := turns[len(turns)-1]
	glance := Glance{State: "working", At: last.At}
	switch {
	case last.Kind == "carla" && last.Text != "":
		glance.State = "idle"
	case last.Kind == "lemon":
		glance.Doing = "just received: " + truncate(last.Text, 200)
	default:
		for i := len(turns) - 1; i >= 0; i-- {
			if turns[i].Kind == "tool" {
				glance.Doing = strings.TrimSpace(turns[i].Name + " " + truncate(turns[i].Summary, 120))
				break
			}
		}
	}
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Kind == "carla" && turns[i].Text != "" {
			glance.Answer = truncate(turns[i].Text, limit)
			break
		}
	}
	return glance
}
