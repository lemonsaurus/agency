package cloud

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/retry"
)

// fakeSSH puts an ssh on PATH that answers master checks, logs every other
// call, and runs script for the rest with $n counting remote commands.
func fakeSSH(t *testing.T, script string) (calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	saved := retry.Delays
	retry.Delays = []time.Duration{0, 0}
	t.Cleanup(func() { retry.Delays = saved })
	log := filepath.Join(dir, "calls")
	body := `#!/bin/sh
case "$*" in *"-O check"*) exit 0 ;; esac
echo "$*" >> ` + log + `
n=$(grep -cv -e "-O exit" -e " true$" ` + log + `)
case "$*" in *"-O exit"*) exit 0 ;; esac
` + script
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return func() []string {
		data, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

func TestRunRetriesUnreachableHost(t *testing.T) {
	fakeSSH(t, `[ "$n" -lt 3 ] && { echo "Connection reset" >&2; exit 255; }
echo listed`)
	var retries []int
	c := &Client{Host: "box", Retrying: func(attempt, attempts int, err error) {
		if !errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "Connection reset") {
			t.Fatal(err)
		}
		retries = append(retries, attempt)
	}}
	out, err := c.Run(context.Background(), time.Second, "list")
	if err != nil || out != "listed\n" || len(retries) != 2 {
		t.Fatalf("out=%q err=%v retries=%v", out, err, retries)
	}
}

func TestRunKeepsRemoteFailures(t *testing.T) {
	calls := fakeSSH(t, `echo "Error: no pane" >&2; exit 1`)
	c := &Client{Host: "box", Retrying: func(int, int, error) { t.Fatal("retried a remote failure") }}
	_, err := c.Run(context.Background(), time.Second, "send")
	if err == nil || errors.Is(err, ErrUnreachable) || len(calls()) != 1 {
		t.Fatalf("err=%v calls=%v", err, calls())
	}
	fakeSSH(t, `exit 255`)
	c.Retrying = func(int, int, error) {}
	if _, err := c.Run(context.Background(), time.Second, "send"); !errors.Is(err, ErrUnreachable) {
		t.Fatal(err)
	}
	c.Retrying = nil
	calls = fakeSSH(t, `exit 255`)
	if _, err := c.Run(context.Background(), time.Second, "send"); !errors.Is(err, ErrUnreachable) || len(calls()) != 1 {
		t.Fatalf("err=%v calls=%v", err, calls())
	}
}

func TestStalledRunDropsDeadMaster(t *testing.T) {
	calls := fakeSSH(t, `case "$*" in *" true") exit 255 ;; esac
[ "$n" -eq 1 ] && exec sleep 10
echo ok`)
	c := &Client{Host: "box", Retrying: func(int, int, error) {}}
	out, err := c.Run(context.Background(), 200*time.Millisecond, "list")
	if err != nil || out != "ok\n" {
		t.Fatalf("out=%q err=%v calls=%v", out, err, calls())
	}
	log := strings.Join(calls(), "\n")
	if !strings.Contains(log, " true") || !strings.Contains(log, "-O exit box") {
		t.Fatalf("stale master kept: %s", log)
	}
}

func TestCopyStreamsThroughSSH(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "source")
	if err := os.WriteFile(local, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	received := filepath.Join(dir, "received")
	calls := fakeSSH(t, `cat > `+received)
	c := &Client{Host: "box"}
	if err := c.Copy(context.Background(), time.Second, local, "/tmp/it's here"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(received)
	if string(data) != "fixture" || !strings.HasSuffix(calls()[0], `box cat > '/tmp/it'\''s here'`) {
		t.Fatalf("data=%q calls=%v", data, calls())
	}
}
