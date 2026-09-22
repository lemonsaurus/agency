package main

import (
	"os"
	"strings"
	"testing"
)

func TestSessionSlug(t *testing.T) {
	got := sessionSlug("/home/lemon/git/journalia/journalia")
	if got != "--home-lemon-git-journalia-journalia--" {
		t.Errorf("unexpected slug %q", got)
	}
}

func TestSwapHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	got, err := swapHome(home+"/git/journalia/journalia", "/home/box")
	if err != nil || got != "/home/box/git/journalia/journalia" {
		t.Errorf("swapHome = %q, %v", got, err)
	}
	if _, err := swapHome("/tmp/elsewhere", "/home/box"); err == nil {
		t.Error("expected error outside home")
	}
}

func TestRemoteCheckout(t *testing.T) {
	script := remoteCheckout("", "/home/box/git/agency", "feature")
	if strings.Contains(script, "worktree add") {
		t.Error("main checkout must not add a worktree")
	}
	if !strings.Contains(script, "git checkout -q -B 'feature' origin/'feature'") {
		t.Errorf("missing checkout: %s", script)
	}
	script = remoteCheckout("/home/box/git/journalia", "/home/box/git/journalia/.worktrees/x", "lemon/x")
	if !strings.Contains(script, "git worktree add -q '/home/box/git/journalia/.worktrees/x' -B 'lemon/x' origin/'lemon/x'") {
		t.Errorf("missing worktree add: %s", script)
	}
}
