package restore

import (
	"os"
	"strings"

	"github.com/lemonsaurus/agency/internal/ipc"
)

// SessionFile finds the Pi session file of a pane: the one its Pi bridge
// announced, else the one its command resumed.
func SessionFile(socket string) func(paneID, command string) string {
	return func(paneID, command string) string {
		if path, err := ipc.SessionPath(socket, paneID); err == nil {
			if data, err := os.ReadFile(path); err == nil {
				if file := strings.TrimSpace(string(data)); file != "" {
					return file
				}
			}
		}
		return commandSession(command)
	}
}

// commandSession is the file after --session in a Pi command line.
func commandSession(command string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		if field == "--session" && i+1 < len(fields) {
			return fields[i+1]
		}
		if value, ok := strings.CutPrefix(field, "--session="); ok {
			return value
		}
	}
	return ""
}

// ResumeCommand is the command that continues a Pi conversation: command
// with any session flag swapped for the saved file.
func ResumeCommand(command, session string) string {
	fields := strings.Fields(command)
	var kept []string
	for i := 0; i < len(fields); i++ {
		switch {
		case fields[i] == "--session":
			i++
		case strings.HasPrefix(fields[i], "--session="):
		default:
			kept = append(kept, fields[i])
		}
	}
	return strings.Join(append(kept, "--session", shellQuote(session)), " ")
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-:+@=,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
