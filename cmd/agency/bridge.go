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
	"syscall"
	"time"

	"github.com/lemonsaurus/agency/internal/control"
	"github.com/lemonsaurus/agency/internal/ipc"
)

func cloudSpawnArgs(args []string, caller control.Requester) []string {
	if caller.Human {
		return append([]string{"spawn", "--role", "manager"}, args[1:]...)
	}
	return args
}

func cancelOnInputClose(input io.Reader, cancel context.CancelFunc) {
	io.Copy(io.Discard, input)
	cancel()
}

func runAsk(args []string) {
	flags := flag.NewFlagSet("ask", flag.ContinueOnError)
	timeout := flags.Duration("timeout", 10*time.Minute, "Maximum wait, up to 30m")
	cancelOnEOF := flags.Bool("cancel-on-stdin-close", false, "Cancel when the SSH client closes command stdin")
	if err := flags.Parse(args); err != nil {
		os.Exit(1)
	}
	if flags.NArg() != 2 || *timeout <= 0 || *timeout > ipc.MaxAskTimeout {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud ask [--timeout 10m] [--cancel-on-stdin-close] <pane-id> <text> (maximum 30m)")
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
		ID: hex.EncodeToString(id), Pane: flags.Arg(0), Text: flags.Arg(1), TimeoutMS: timeout.Milliseconds(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if reply.Error != nil {
		fmt.Fprintf(os.Stderr, "Error [%s]: %s\n", reply.Error.Code, reply.Error.Message)
		os.Exit(1)
	}
	fmt.Print(*reply.Text)
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
