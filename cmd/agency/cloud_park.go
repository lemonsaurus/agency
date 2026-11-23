package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// parkCaptures are the waits before each try at the window's text. A host
// under load times captures out, so the screen draws at once and fills in.
var parkCaptures = []time.Duration{0, 2 * time.Second, 5 * time.Second, 10 * time.Second}

// runParkScreen runs on the host inside a park session: it draws the window's
// text as it is now, grey, and redraws on resize. A parked viewer shows it
// until its device shows the viewer again.
func runParkScreen(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud park-screen <window-id>")
		os.Exit(1)
	}
	captured := make(chan string, 1)
	go func() {
		tc := tmux.NewClient("", "")
		for _, wait := range parkCaptures {
			time.Sleep(wait)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			text := tc.CaptureWindow(ctx, args[0])
			cancel()
			if text != "" {
				captured <- text
				return
			}
		}
	}()
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	text := ""
	for {
		width, height, err := term.GetSize(os.Stdout.Fd())
		if err != nil {
			width, height = 80, 24
		}
		fmt.Print(cloud.ParkFrame(text, width, height))
		select {
		case <-resize:
		case text = <-captured:
		}
	}
}
