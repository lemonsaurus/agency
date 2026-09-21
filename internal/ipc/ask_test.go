package ipc

import (
	"bufio"
	"context"
	"encoding/json"
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
