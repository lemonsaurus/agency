package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/control"
)

func askFixture(t *testing.T, requester control.Requester, respond func(net.Conn, AskRequest)) (string, AskRequest) {
	t.Helper()
	home, err := os.MkdirTemp("", "ask-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	socket := filepath.Join(home, "daemon.sock")
	server := NewServer(socket, &mockHandler{requester: requester})
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	path, err := BridgePath(socket, "%9")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		line, _ := reader.ReadString('\n')
		var request AskRequest
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "ask:")), &request) != nil {
			return
		}
		respond(conn, request)
	}()
	return socket, AskRequest{ID: strings.Repeat("a", 32), Pane: "%9", Text: "say hi\nUnicode 🐍", TimeoutMS: 5000}
}

func TestTranscript(t *testing.T) {
	dir, err := os.MkdirTemp("", "transcript-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	request := TranscriptRequest{ID: strings.Repeat("b", 32), Limit: 200}
	for _, tt := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{"messages", `{"id":"` + request.ID + `","entries":[{"role":"user","at":1727000000000,"blocks":[{"type":"text","text":"hello"}]}]}`, ""},
		{"empty", `{"id":"` + request.ID + `","entries":[]}`, ""},
		{"large", `{"id":"` + request.ID + `","entries":[{"role":"assistant","blocks":[{"type":"text","text":"` + strings.Repeat("a", 9*1024*1024) + `"}]}]}`, ""},
		{"error", `{"id":"` + request.ID + `","error":{"code":"unavailable","message":"closed"}}`, ""},
		{"mismatched ID", `{"id":"wrong","entries":[]}`, "invalid bridge reply"},
		{"malformed", `not JSON`, "invalid bridge reply"},
		{"missing entries", `{"id":"` + request.ID + `"}`, "invalid bridge reply"},
		{"null entries", `{"id":"` + request.ID + `","entries":null}`, "invalid bridge reply"},
		{"both entries and error", `{"id":"` + request.ID + `","entries":[],"error":{"code":"bad"}}`, "invalid bridge reply"},
		{"disconnected", "", "bridge disconnected"},
		{"oversize", strings.Repeat("a", 16*1024*1024), "token too long"},
		{"timeout", "", "context deadline exceeded"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, "pane.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			input := make(chan string, 1)
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				line, _ := bufio.NewReader(conn).ReadString('\n')
				input <- line
				if tt.name == "timeout" {
					io.Copy(io.Discard, conn)
				} else if tt.body != "" {
					fmt.Fprintln(conn, tt.body)
				}
			}()
			timeout := 5 * time.Second
			if tt.name == "timeout" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			reply, err := Transcript(ctx, path, request)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want %q", err, tt.wantErr)
				}
				if tt.name == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			} else if err != nil || reply.ID != request.ID {
				t.Fatalf("id=%q err=%v", reply.ID, err)
			} else if tt.name == "error" {
				if reply.Error == nil || reply.Error.Code != "unavailable" {
					t.Fatal(reply)
				}
			} else {
				encoded, err := json.Marshal(reply)
				if err != nil || string(encoded) != tt.body {
					t.Fatal("reply changed")
				}
			}
			select {
			case line := <-input:
				want := `transcript:{"id":"` + request.ID + `","limit":200}` + "\n"
				if line != want {
					t.Fatalf("request=%q", line)
				}
			case <-time.After(time.Second):
				t.Fatal("no request")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("connection remained open")
			}
		})
	}
	for _, invalid := range []TranscriptRequest{{ID: "bad", Limit: 200}, {ID: request.ID, Limit: 0}, {ID: request.ID, Limit: 501}} {
		if _, err := Transcript(context.Background(), "", invalid); err == nil || !strings.Contains(err.Error(), "invalid transcript") {
			t.Fatalf("accepted %+v: %v", invalid, err)
		}
	}
	if _, err := Transcript(context.Background(), filepath.Join(dir, "missing.sock"), request); err == nil || err.Error() != "bridge unavailable; update Agency and /reload Pi in the target pane" {
		t.Fatal(err)
	}
}

func TestAskAttributionAndExactReply(t *testing.T) {
	for _, human := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker", true: "human"}[human], func(t *testing.T) {
			input := make(chan AskRequest, 1)
			answer := "hello\n\nworld 🐍\n"
			socket, request := askFixture(t, control.Requester{PaneID: "%2", Role: control.RoleWorker, Human: human}, func(conn net.Conn, r AskRequest) {
				input <- r
				json.NewEncoder(conn).Encode(AskReply{ID: r.ID, Text: &answer})
			})
			reply, err := Ask(context.Background(), socket, request)
			if err != nil || reply.Text == nil || *reply.Text != answer {
				t.Fatalf("reply=%+v err=%v", reply, err)
			}
			got := <-input
			want := request.Text
			if !human {
				want = "[from %2 worker] " + want
			}
			if got.Text != want || got.ID != request.ID {
				t.Fatalf("request=%+v", got)
			}
		})
	}
}

func TestAskDetachedReply(t *testing.T) {
	input := make(chan AskRequest, 1)
	socket, request := askFixture(t, control.Requester{Human: true}, func(conn net.Conn, r AskRequest) {
		input <- r
		json.NewEncoder(conn).Encode(AskReply{ID: r.ID, Accepted: true})
	})
	request.Detach = true
	reply, err := Ask(context.Background(), socket, request)
	if err != nil || !reply.Accepted || reply.Text != nil {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
	if got := <-input; !got.Detach {
		t.Fatalf("request=%+v", got)
	}
	for _, bad := range []AskReply{{}, {Accepted: true, Error: &AskError{Code: "x"}}} {
		if bad.valid() {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestAskDisconnectAndTimeout(t *testing.T) {
	for _, timedOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnect", true: "timeout"}[timedOut], func(t *testing.T) {
			connected := make(chan struct{})
			closed := make(chan struct{})
			socket, request := askFixture(t, control.Requester{}, func(conn net.Conn, _ AskRequest) {
				close(connected)
				io.Copy(io.Discard, conn)
				close(closed)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if timedOut {
				request.TimeoutMS = 40
			}
			finished := make(chan AskReply, 1)
			go func() { reply, _ := Ask(ctx, socket, request); finished <- reply }()
			<-connected
			if !timedOut {
				cancel()
			}
			select {
			case reply := <-finished:
				if timedOut && (reply.Error == nil || reply.Error.Code != "timeout") {
					t.Fatalf("reply=%+v", reply)
				}
			case <-time.After(time.Second):
				t.Fatal("ask did not stop")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("Pi connection remained open")
			}
		})
	}
}

func TestAskRejectsInvalidAndSelf(t *testing.T) {
	socket, request := askFixture(t, control.Requester{PaneID: "%9", Role: control.RoleWorker}, func(net.Conn, AskRequest) { t.Error("should not reach Pi") })
	reply, err := Ask(context.Background(), socket, request)
	if err != nil || reply.Error == nil || reply.Error.Code != "busy" {
		t.Fatal(reply, err)
	}
	request.Pane = "../escape"
	reply, err = Ask(context.Background(), socket, request)
	if err != nil || reply.Error == nil || reply.Error.Code != "invalid_request" {
		t.Fatal(reply, err)
	}
	if _, err := BridgePath(socket, request.Pane); err == nil {
		t.Fatal("accepted invalid pane")
	}
}

func TestSendMessageContextCancelsRead(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "cap.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		bufio.NewReader(conn).ReadString('\n')
		cancel()
		io.Copy(io.Discard, conn)
	}()
	if _, err := SendMessageContext(ctx, socket, "capabilities"); err == nil {
		t.Fatal("capability wait ignored cancellation")
	}
}

func TestAskDoesNotReturnUncorrelatedReply(t *testing.T) {
	socket, request := askFixture(t, control.Requester{}, func(conn net.Conn, _ AskRequest) { io.WriteString(conn, `{"id":"wrong","text":"other turn"}`+"\n") })
	reply, err := Ask(context.Background(), socket, request)
	if err != nil || reply.Error == nil || !strings.Contains(reply.Error.Message, "invalid bridge reply") {
		t.Fatal(reply, err)
	}
}
