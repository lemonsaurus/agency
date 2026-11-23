package ipc

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

type blockedRelayout struct {
	*mockHandler
	entered chan struct{}
	release chan struct{}
}

func (h *blockedRelayout) Relayout(ctx context.Context) error {
	h.entered <- struct{}{}
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestServerBoundsConnections(t *testing.T) {
	h := &blockedRelayout{mockHandler: &mockHandler{}, entered: make(chan struct{}, 3), release: make(chan struct{})}
	s := NewServer(filepath.Join(t.TempDir(), "test.sock"), h)
	s.slots = make(chan struct{}, 2)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for i := 0; i < 3; i++ {
		conn, err := net.Dial("unix", s.Path())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		fmt.Fprintln(conn, "relayout")
		if i < 2 {
			select {
			case <-h.entered:
			case <-time.After(time.Second):
				t.Fatal("handler did not start")
			}
		}
	}
	select {
	case <-h.entered:
		t.Fatal("third handler exceeded the connection limit")
	case <-time.After(50 * time.Millisecond):
	}
	h.release <- struct{}{}
	select {
	case <-h.entered:
	case <-time.After(time.Second):
		t.Fatal("released slot was not reused")
	}
}

func TestServerCancelClosesPartialRequest(t *testing.T) {
	s := NewServer("", &mockHandler{})
	defer s.cancel()
	server, client := net.Pipe()
	defer client.Close()
	finished := make(chan struct{})
	go func() {
		s.handleConn(server, func() {})
		close(finished)
	}()
	client.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := client.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	s.cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("connection remained blocked after cancellation")
	}
}

func TestServerExpiresIdleRequest(t *testing.T) {
	saved := requestReadTimeout
	requestReadTimeout = 20 * time.Millisecond
	t.Cleanup(func() { requestReadTimeout = saved })
	s := NewServer("", &mockHandler{})
	defer s.cancel()
	server, client := net.Pipe()
	defer client.Close()
	finished := make(chan struct{})
	go func() {
		s.handleConn(server, func() {})
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("idle request did not expire")
	}
}

type waitingCloud struct {
	*mockHandler
	waiting chan struct{}
}

func (h *waitingCloud) Cloud(ctx context.Context, _ string) (string, error) {
	h.waiting <- struct{}{}
	<-ctx.Done()
	return "", ctx.Err()
}

func TestCloudWaitsTakeNoSlotAndEndOnHangup(t *testing.T) {
	h := &waitingCloud{mockHandler: &mockHandler{}, waiting: make(chan struct{}, 3)}
	s := NewServer(filepath.Join(t.TempDir(), "test.sock"), h)
	s.slots = make(chan struct{}, 1)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var viewers []net.Conn
	for range 3 {
		conn, err := net.Dial("unix", s.Path())
		if err != nil {
			t.Fatal(err)
		}
		viewers = append(viewers, conn)
		fmt.Fprintln(conn, "cloud:gate @1 %1")
		select {
		case <-h.waiting:
		case <-time.After(time.Second):
			t.Fatal("a waiting viewer held the only slot")
		}
	}
	if reply, err := SendMessage(s.Path(), "relayout"); err != nil || reply != "ok" {
		t.Fatalf("request behind waiting viewers = %q, %v", reply, err)
	}
	for _, conn := range viewers {
		conn.Close()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	s.listener.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("hung-up viewers kept waiting")
	}
}
