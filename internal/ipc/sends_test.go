package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/control"
)

func TestSendLedgerJoinsRetriesOfAnInFlightSend(t *testing.T) {
	var ledger sendLedger
	key := sendKey("%2", "%9", "run the tests", true)
	release := make(chan struct{})
	var calls atomic.Int32
	send := func() error {
		calls.Add(1)
		<-release
		return nil
	}
	first := make(chan time.Time)
	go func() {
		at, _ := ledger.deliver(context.Background(), key, send)
		first <- at
	}()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	var wg sync.WaitGroup
	retries := make(chan time.Time, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			at, err := ledger.deliver(context.Background(), key, send)
			if err != nil {
				t.Error(err)
			}
			retries <- at
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(retries)
	if at := <-first; !at.IsZero() {
		t.Fatalf("the first send reported an earlier delivery at %v", at)
	}
	for at := range retries {
		if at.IsZero() {
			t.Fatal("a retry delivered again instead of joining")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("send ran %d times, want 1", calls.Load())
	}
}

func TestSendLedgerSendsAgainAfterFailureOrWindow(t *testing.T) {
	var ledger sendLedger
	key := sendKey("%2", "%9", "run the tests", true)
	calls := 0
	if _, err := ledger.deliver(context.Background(), key, func() error { calls++; return errors.New("can't find pane") }); err == nil {
		t.Fatal("expected the failure")
	}
	if at, err := ledger.deliver(context.Background(), key, func() error { calls++; return nil }); err != nil || !at.IsZero() {
		t.Fatalf("a retry after a failure must send: %v %v", at, err)
	}
	if at, _ := ledger.deliver(context.Background(), key, func() error { calls++; return nil }); at.IsZero() {
		t.Fatal("a retry after a delivery must not send")
	}
	ledger.entries[key].at = time.Now().Add(-ResendWindow - time.Second)
	if at, _ := ledger.deliver(context.Background(), key, func() error { calls++; return nil }); !at.IsZero() {
		t.Fatal("a send after the window must send")
	}
	if other, _ := ledger.deliver(context.Background(), sendKey("%2", "%8", "run the tests", true), func() error { calls++; return nil }); !other.IsZero() {
		t.Fatal("the same text to another pane is a new message")
	}
	if calls != 4 {
		t.Fatalf("send ran %d times, want 4", calls)
	}
}

func TestSendLedgerStopsWaitingWhenTheCallerLeaves(t *testing.T) {
	var ledger sendLedger
	key := sendKey("%2", "%9", "x", true)
	release := make(chan struct{})
	defer close(release)
	go ledger.deliver(context.Background(), key, func() error { <-release; return nil })
	for {
		ledger.mu.Lock()
		started := ledger.entries[key] != nil
		ledger.mu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := ledger.deliver(ctx, key, func() error { t.Error("joined send ran"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the caller's deadline", err)
	}
}

// sendServer starts a daemon socket whose requester is an agent in %2, and,
// unless reply is nil, a Pi bridge for %9 that answers each send with reply.
func sendServer(t *testing.T, requester control.Requester, reply func(conn net.Conn, id string)) (string, *mockHandler, *atomic.Int32) {
	t.Helper()
	home, err := os.MkdirTemp("", "sends-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	socket := filepath.Join(home, "daemon.sock")
	handler := &mockHandler{requester: requester}
	server := NewServer(socket, handler)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	var bridged atomic.Int32
	if reply != nil {
		path, _ := BridgePath(socket, "%9")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					line, _ := bufio.NewReader(conn).ReadString('\n')
					var request SendRequest
					json.Unmarshal([]byte(strings.TrimPrefix(line, "send:")), &request)
					bridged.Add(1)
					reply(conn, request.ID)
				}()
			}
		}()
	}
	return socket, handler, &bridged
}

func send(t *testing.T, socket string, payload sendPayload) string {
	t.Helper()
	data, _ := json.Marshal(payload)
	reply, err := SendMessage(socket, "send:"+string(data))
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestSendBridgeOutcomes(t *testing.T) {
	old := bridgeSendTimeout
	bridgeSendTimeout = 50 * time.Millisecond
	t.Cleanup(func() { bridgeSendTimeout = old })
	agent := control.Requester{PaneID: "%2", Role: control.RoleWorker}
	for _, tt := range []struct {
		name  string
		reply func(conn net.Conn, id string)
		typed bool
	}{
		{"accepted", func(conn net.Conn, id string) { conn.Write([]byte(`{"id":"` + id + `","accepted":true}` + "\n")) }, false},
		{"stalled past the timeout", func(net.Conn, string) { time.Sleep(200 * time.Millisecond) }, false},
		{"closed without replying", func(net.Conn, string) {}, false},
		{"refused by an old Pi", func(conn net.Conn, _ string) {
			conn.Write([]byte(`{"error":{"code":"invalid_request","message":"Invalid bridge request"}}` + "\n"))
		}, true},
		{"no bridge", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			socket, handler, bridged := sendServer(t, agent, tt.reply)
			if reply := send(t, socket, sendPayload{Pane: "%9", Text: "run the tests", Enter: true}); reply != "ok" {
				t.Fatalf("reply %q", reply)
			}
			handler.mu.Lock()
			defer handler.mu.Unlock()
			if typed := len(handler.sends) == 1; typed != tt.typed || len(handler.sends) > 1 {
				t.Fatalf("typed %d times, want typed %v", len(handler.sends), tt.typed)
			}
			if want := int32(1); tt.reply != nil && bridged.Load() != want {
				t.Fatalf("bridge got %d sends, want %d", bridged.Load(), want)
			}
		})
	}
}

func TestSendRetryIsDeliveredOnce(t *testing.T) {
	agent := control.Requester{PaneID: "%2", Role: control.RoleWorker}
	human := control.Requester{PaneID: "%0", Role: control.RoleController, Human: true}
	for _, tt := range []struct {
		name      string
		requester control.Requester
		again     bool
		typed     int
	}{
		{"agent retry", agent, false, 1},
		{"agent --again", agent, true, 2},
		{"human", human, false, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			socket, handler, _ := sendServer(t, tt.requester, nil)
			payload := sendPayload{Pane: "%9", Text: "run the tests", Enter: true}
			if reply := send(t, socket, payload); reply != "ok" {
				t.Fatalf("first reply %q", reply)
			}
			payload.Again = tt.again
			reply := send(t, socket, payload)
			if absorbed := strings.HasPrefix(reply, AlreadySent); absorbed != (tt.typed == 1) {
				t.Fatalf("second reply %q", reply)
			}
			if at, ok := strings.CutPrefix(reply, AlreadySent); ok {
				if _, err := time.Parse(time.RFC3339, at); err != nil {
					t.Fatalf("delivery time %q: %v", at, err)
				}
			}
			handler.mu.Lock()
			defer handler.mu.Unlock()
			if len(handler.sends) != tt.typed {
				t.Fatalf("typed %d times, want %d", len(handler.sends), tt.typed)
			}
		})
	}
}
