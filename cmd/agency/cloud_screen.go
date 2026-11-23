package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/ipc"
)

// runSkyScreen draws the sky screen in the placeholder pane: who holds the
// sky harness, how the link stands, or the empty world's help. It redraws
// every second and on resize, and Enter does what the screen offers: take
// over, reconnect, or re-sync.
func runSkyScreen(cfg *config.Config) {
	sock := socketPath(cfg.Session.Name)
	if state, err := term.MakeRaw(os.Stdin.Fd()); err == nil {
		defer term.Restore(os.Stdin.Fd(), state)
	}
	enter := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			if bytes.ContainsAny(buf[:n], "\r\n") {
				select {
				case enter <- struct{}{}:
				default:
				}
			}
		}
	}()
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	hint := fmt.Sprintf("Prefix+%s opens a shell there  ·  Prefix+%s picks an agent", cfg.Keys.Terminal, cfg.Keys.Palette)
	note, noteUntil := "", time.Time{}
	draw := func() {
		now := time.Now()
		if now.After(noteUntil) {
			note = ""
		}
		width, height, err := term.GetSize(os.Stdout.Fd())
		if err != nil {
			width, height = 80, 24
		}
		fmt.Print(cloud.Screen(skyStatus(sock), cfg.Cloud.Host, hint, note, now, width, height))
	}
	for {
		draw()
		select {
		case <-resize:
		case <-time.After(time.Second):
		case <-enter:
			note, noteUntil = "⟳  one moment…", time.Now().Add(time.Minute)
			draw()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			reply, err := ipc.SendMessageContext(ctx, sock, "cloud:enter")
			cancel()
			switch {
			case err != nil:
				note = "✕  " + err.Error()
			case strings.HasPrefix(reply, "error:"):
				note = "✕  " + strings.TrimSpace(strings.TrimPrefix(reply, "error:"))
			default:
				note = "⟳  " + reply
			}
			noteUntil = time.Now().Add(3 * time.Second)
		}
	}
}

// skyStatus asks the daemon how the link stands. A daemon that does not
// answer reads as a link still connecting, with the reason.
func skyStatus(sock string) cloud.LinkStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := ipc.SendMessageContext(ctx, sock, "cloud:status")
	var status cloud.LinkStatus
	if err == nil && !strings.HasPrefix(reply, "error:") {
		if err = json.Unmarshal([]byte(reply), &status); err == nil {
			return status
		}
	}
	if err == nil {
		err = fmt.Errorf("%s", strings.TrimSpace(strings.TrimPrefix(reply, "error:")))
	}
	return cloud.LinkStatus{State: cloud.LinkConnecting, Attempts: cloud.LinkAttempts, Here: true, Windows: -1, Error: "Agency is not answering: " + err.Error()}
}
