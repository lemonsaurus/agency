package agents

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/lemonsaurus/agency/internal/config"
)

// Registry holds agent type definitions and provides lookup methods.
type Registry struct {
	agents map[string]config.AgentConfig
	order  []string
}

// NewRegistry creates a registry from a config's agent map.
// If agentOrder is provided, it determines iteration order; otherwise alphabetical.
func NewRegistry(agents map[string]config.AgentConfig, agentOrder []string) *Registry {
	order := make([]string, 0, len(agents))
	if len(agentOrder) > 0 {
		// Use provided order, but only include names that exist in the map.
		seen := make(map[string]bool)
		for _, name := range agentOrder {
			if _, ok := agents[name]; ok && !seen[name] {
				order = append(order, name)
				seen[name] = true
			}
		}
		// Append any agents not in the provided order.
		for name := range agents {
			if !seen[name] {
				order = append(order, name)
			}
		}
	} else {
		for name := range agents {
			order = append(order, name)
		}
		sort.Strings(order)
	}
	return &Registry{agents: agents, order: order}
}

// Get returns the agent config for a given name, and whether it exists.
func (r *Registry) Get(name string) (config.AgentConfig, bool) {
	a, ok := r.agents[name]
	return a, ok
}

// Names returns agent names in their configured order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// DetectType matches a running command line against known agent commands.
// It returns the agent name if matched, or empty string.
func (r *Registry) DetectType(cmdline string) string {
	if cmdline == "" {
		return ""
	}
	// Extract the base command (first token, basename only).
	fields := strings.Fields(cmdline)
	if len(fields) == 0 {
		return ""
	}
	base := filepath.Base(fields[0])

	for _, name := range r.order {
		agent := r.agents[name]
		agentCmd := strings.Fields(agent.Command)
		if len(agentCmd) == 0 {
			continue
		}
		agentBase := filepath.Base(agentCmd[0])
		if base == agentBase {
			return name
		}
	}
	return ""
}
