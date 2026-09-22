package ipc

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const MaxAskBytes = 128 * 1024
const MaxAskTimeout = 30 * time.Minute

var paneIDPattern = regexp.MustCompile(`^%[0-9]+$`)
var requestIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type AskRequest struct {
	ID        string `json:"id"`
	Pane      string `json:"pane"`
	Text      string `json:"text"`
	TimeoutMS int64  `json:"timeoutMs"`
	Detach    bool   `json:"detach,omitempty"`
}

type AskError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type AskReply struct {
	ID    string    `json:"id"`
	Text     *string   `json:"text,omitempty"`
	Accepted bool      `json:"accepted,omitempty"`
	Error    *AskError `json:"error,omitempty"`
}

func BridgePath(daemonSocket, paneID string) (string, error) {
	if !paneIDPattern.MatchString(paneID) {
		return "", fmt.Errorf("invalid pane ID")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(daemonSocket))
	return filepath.Join(home, ".agents", "run", "agency", fmt.Sprintf("%x", hash[:8]), paneID[1:]+".sock"), nil
}

// Exactly one of text, accepted, or error.
func (r AskReply) valid() bool {
	count := 0
	if r.Text != nil {
		count++
	}
	if r.Accepted {
		count++
	}
	if r.Error != nil {
		count++
	}
	return count == 1
}

// Ask uses one LF-delimited request and reply per connection. Closing cancels it.
func Ask(ctx context.Context, socket string, request AskRequest) (AskReply, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return AskReply{}, fmt.Errorf("bridge unavailable; update Agency and /reload Pi in the target pane")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	data, err := json.Marshal(request)
	if err != nil {
		return AskReply{}, err
	}
	if len(data)+5 > MaxAskBytes {
		return AskReply{}, fmt.Errorf("request exceeds %d bytes", MaxAskBytes)
	}
	if _, err := fmt.Fprintf(conn, "ask:%s\n", data); err != nil {
		return AskReply{}, err
	}
	reader := bufio.NewScanner(conn)
	reader.Buffer(make([]byte, 4096), 8*1024*1024)
	if !reader.Scan() {
		if ctx.Err() != nil {
			return AskReply{}, ctx.Err()
		}
		return AskReply{}, fmt.Errorf("bridge disconnected before replying")
	}
	var reply AskReply
	if err := json.Unmarshal(reader.Bytes(), &reply); err != nil || reply.ID != request.ID || !reply.valid() {
		return AskReply{}, fmt.Errorf("invalid bridge reply")
	}
	return reply, nil
}

type TranscriptRequest struct {
	ID    string `json:"id"`
	Limit int    `json:"limit"`
}

type TranscriptReply struct {
	ID      string            `json:"id"`
	Entries []json.RawMessage `json:"entries"`
	Error   *AskError         `json:"error,omitempty"`
}

func Transcript(ctx context.Context, socket string, request TranscriptRequest) (TranscriptReply, error) {
	if !requestIDPattern.MatchString(request.ID) || request.Limit < 1 || request.Limit > 500 {
		return TranscriptReply{}, fmt.Errorf("invalid transcript ID or limit (1..500)")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return TranscriptReply{}, fmt.Errorf("bridge unavailable; update Agency and /reload Pi in the target pane")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	data, err := json.Marshal(request)
	if err != nil {
		return TranscriptReply{}, err
	}
	if _, err := fmt.Fprintf(conn, "transcript:%s\n", data); err != nil {
		return TranscriptReply{}, err
	}
	reader := bufio.NewScanner(conn)
	reader.Buffer(make([]byte, 4096), 16*1024*1024)
	if !reader.Scan() {
		if ctx.Err() != nil {
			return TranscriptReply{}, ctx.Err()
		}
		if reader.Err() != nil {
			return TranscriptReply{}, reader.Err()
		}
		return TranscriptReply{}, fmt.Errorf("bridge disconnected before replying")
	}
	var reply TranscriptReply
	if err := json.Unmarshal(reader.Bytes(), &reply); err != nil || reply.ID != request.ID || (reply.Entries == nil) == (reply.Error == nil) {
		return TranscriptReply{}, fmt.Errorf("invalid bridge reply")
	}
	return reply, nil
}

func (s *Server) handleAsk(conn net.Conn, reader io.Reader, payload string) {
	var request AskRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		json.NewEncoder(conn).Encode(AskReply{Error: &AskError{Code: "invalid_request", Message: "invalid ask request"}})
		return
	}
	reply := AskReply{ID: request.ID}
	fail := func(code, message string) {
		reply.Error = &AskError{Code: code, Message: message}
		json.NewEncoder(conn).Encode(reply)
	}
	if !requestIDPattern.MatchString(request.ID) || !paneIDPattern.MatchString(request.Pane) || strings.TrimSpace(request.Text) == "" || request.TimeoutMS <= 0 || request.TimeoutMS > MaxAskTimeout.Milliseconds() {
		fail("invalid_request", "invalid ask ID, pane, text, or timeout (maximum 30m)")
		return
	}
	requester, err := s.requester(peerPIDForConn(conn))
	if err != nil {
		fail("unauthorized", err.Error())
		return
	}
	if requester.PaneID == request.Pane && !requester.Human {
		fail("busy", "a pane cannot ask itself")
		return
	}
	if !requester.Human {
		request.Text = fmt.Sprintf("[from %s %s] %s", requester.PaneID, requester.Role, request.Text)
	}
	path, err := BridgePath(s.path, request.Pane)
	if err != nil {
		fail("invalid_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(request.TimeoutMS)*time.Millisecond)
	defer cancel()
	go func() {
		io.Copy(io.Discard, reader)
		cancel()
	}()
	reply, err = Ask(ctx, path, request)
	if err != nil {
		reply.ID = request.ID
		code := "unavailable"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = "timeout"
		} else if ctx.Err() != nil {
			code = "canceled"
		}
		fail(code, err.Error())
		return
	}
	json.NewEncoder(conn).Encode(reply)
}
