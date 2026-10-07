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

// runParkScreen runs on the host inside a park session: it draws the window's
// text as it is now, grey, and redraws on resize. A parked viewer shows it
// until its device shows the viewer again.
func runParkScreen(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud park-screen <window-id>")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	text := tmux.NewClient("", "").CaptureWindow(ctx, args[0])
	cancel()
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	for {
		width, height, err := term.GetSize(os.Stdout.Fd())
		if err != nil {
			width, height = 80, 24
		}
		fmt.Print(cloud.ParkFrame(text, width, height))
		<-resize
	}
}
