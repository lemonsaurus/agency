package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/ipc"
	"github.com/lemonsaurus/agency/internal/tmux"
)

func cancelOnInputClose(input io.Reader, cancel context.CancelFunc) {
	io.Copy(io.Discard, input)
	cancel()
}

func runAsk(args []string) {
	flags := flag.NewFlagSet("ask", flag.ContinueOnError)
	timeout := flags.Duration("timeout", 10*time.Minute, "Maximum wait, up to 30m")
	cancelOnEOF := flags.Bool("cancel-on-stdin-close", false, "Cancel when the SSH client closes command stdin")
	detach := flags.Bool("detach", false, "Return once the pane starts the turn; the turn keeps running")
	if err := flags.Parse(args); err != nil {
		os.Exit(1)
	}
	if flags.NArg() != 2 || *timeout <= 0 || *timeout > ipc.MaxAskTimeout {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud ask [--timeout 10m] [--cancel-on-stdin-close] [--detach] <pane-id> <text> (maximum 30m)")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout+time.Second)
	defer cancel()
	if *cancelOnEOF {
		go cancelOnInputClose(os.Stdin, cancel)
	}
	socket := socketPath(loadConfig().Session.Name)
	capabilityContext, cancelCapability := context.WithTimeout(ctx, 5*time.Second)
	defer cancelCapability()
	capabilitiesJSON, err := ipc.SendMessageContext(capabilityContext, socket, "capabilities")
	var capabilities control.Capabilities
	if err != nil || json.Unmarshal([]byte(capabilitiesJSON), &capabilities) != nil || !capabilities.PromptBridge {
		fmt.Fprintln(os.Stderr, "Error: running Agency daemon lacks the prompt bridge; install the updated binary and restart agency.service")
		os.Exit(1)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		fmt.Fprintln(os.Stderr, "Error generating request ID")
		os.Exit(1)
	}
	reply, err := ipc.Ask(ctx, socket, ipc.AskRequest{
		ID: hex.EncodeToString(id), Pane: flags.Arg(0), Text: flags.Arg(1), TimeoutMS: timeout.Milliseconds(), Detach: *detach,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if reply.Error != nil {
		fmt.Fprintf(os.Stderr, "Error [%s]: %s\n", reply.Error.Code, reply.Error.Message)
		os.Exit(1)
	}
	if reply.Accepted {
		fmt.Println("accepted")
		return
	}
	fmt.Print(*reply.Text)
}

func runTranscript(args []string) {
	flags := flag.NewFlagSet("transcript", flag.ContinueOnError)
	limit := flags.Int("limit", 200, "Messages to return, from 1 to 500")
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud transcript <pane-id> [--limit 200]")
		os.Exit(1)
	}
	if err := flags.Parse(args[1:]); err != nil {
		os.Exit(1)
	}
	if flags.NArg() != 0 || *limit < 1 || *limit > 500 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud transcript <pane-id> [--limit 200] (limit 1..500)")
		os.Exit(1)
	}
	path, err := ipc.BridgePath(socketPath(loadConfig().Session.Name), args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		fmt.Fprintln(os.Stderr, "Error generating request ID")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	reply, err := ipc.Transcript(ctx, path, ipc.TranscriptRequest{ID: hex.EncodeToString(id), Limit: *limit})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	if reply.Error != nil {
		fmt.Fprintf(os.Stderr, "Error [%s]: %s\n", reply.Error.Code, reply.Error.Message)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(reply); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// runSendToSky moves a window to the sky harness. Idle Pi panes run their own
// /handoff-cloud, which serializes them; shells reopen in the same folder in
// the matching sky window.
func runSendToSky(args []string) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "Usage: agency send-to-sky <client> <window-id>")
		os.Exit(1)
	}
	cfg := loadConfig()
	socket := socketPath(cfg.Session.Name)
	tc := tmux.NewClient(cfg.Session.Name, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := tc.Cmd.Run(ctx, "list-panes", "-t", args[1], "-F", "#{pane_id}\t#{pane_current_command}\t#{pane_current_path}\t#{window_name}")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	remote := &cloud.Client{Host: cfg.Cloud.Host}
	remoteHome := ""
	sent, shells := 0, 0
	var skipped []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			continue
		}
		pane, command, dir, window := fields[0], fields[1], fields[2], fields[3]
		path, err := ipc.BridgePath(socket, pane)
		if err != nil {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			id := make([]byte, 16)
			if _, err := rand.Read(id); err != nil {
				fmt.Fprintln(os.Stderr, "Error generating request ID")
				os.Exit(1)
			}
			reply, err := ipc.Command(ctx, path, ipc.CommandRequest{ID: hex.EncodeToString(id), Name: "handoff-cloud"})
			switch {
			case err != nil:
				skipped = append(skipped, fmt.Sprintf("%s (%v)", pane, err))
			case reply.Error != nil:
				skipped = append(skipped, fmt.Sprintf("%s (%s)", pane, reply.Error.Code))
			default:
				sent++
			}
			continue
		}
		if command != filepath.Base(os.Getenv("SHELL")) {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", pane, command))
			continue
		}
		if remoteHome == "" {
			home, err := remote.Shell(ctx, 20*time.Second, "echo \"$HOME\"")
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s (%v)", pane, err))
				continue
			}
			remoteHome = strings.TrimSpace(home)
		}
		remoteDir, err := swapHome(dir, remoteHome)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", pane, err))
			continue
		}
		if _, err := remote.Run(ctx, 20*time.Second, "spawn", "--window", window, "--cmd", "$SHELL", remoteDir); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", pane, err))
			continue
		}
		if resp, err := ipc.SendMessage(socket, "kill:"+pane); err != nil || strings.HasPrefix(resp, "error:") {
			_, _ = tc.Cmd.Run(ctx, "kill-pane", "-t", pane)
		}
		shells++
	}
	if shells > 0 {
		_, _ = ipc.SendMessage(socket, "sync-cloud")
	}
	message := fmt.Sprintf("☁  Sending %d Pi panes and %d shells to the sky", sent, shells)
	if len(skipped) > 0 {
		message += "; skipped " + strings.Join(skipped, ", ")
	}
	_, _ = tc.Cmd.Run(ctx, "display-message", "-c", args[0], "-d", "8000", message)
}

func runBridgePath() {
	socket := socketPath(loadConfig().Session.Name)
	out, err := ipc.SendMessage(socket, "whoami")
	var caller control.Requester
	if err != nil || json.Unmarshal([]byte(out), &caller) != nil || caller.Human || caller.PaneID == "" {
		fmt.Fprintln(os.Stderr, "Error: Pi bridge requires an authenticated Agency pane")
		os.Exit(1)
	}
	path, err := ipc.BridgePath(socket, caller.PaneID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	fmt.Println(path)
}
