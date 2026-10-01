package cloud

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type attachmentMock struct {
	dir        string
	scripts    []string
	copies     [][2]string
	sends      [][]string
	copyError  int
	chmodError bool
	cleanError bool
	sendError  bool
}

func (m *attachmentMock) Shell(ctx context.Context, timeout time.Duration, script string) (string, error) {
	m.scripts = append(m.scripts, script)
	switch {
	case strings.Contains(script, "mktemp"):
		return m.dir + "\n", nil
	case m.chmodError && strings.HasPrefix(script, "chmod"):
		return "", errors.New("chmod failed")
	case m.cleanError && strings.HasPrefix(script, "rm"):
		return "", errors.New("cleanup failed")
	}
	return "", nil
}

func (m *attachmentMock) Copy(ctx context.Context, timeout time.Duration, local, remote string) error {
	m.copies = append(m.copies, [2]string{local, remote})
	if m.copyError == len(m.copies) {
		return errors.New("connection lost")
	}
	return ctx.Err()
}

func (m *attachmentMock) Run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	m.sends = append(m.sends, args)
	if m.sendError {
		return "", errors.New("send failed")
	}
	return "", nil
}

func attachmentFiles(t *testing.T) []string {
	t.Helper()
	var paths []string
	for range 2 {
		path := filepath.Join(t.TempDir(), "report ' $(touch never).txt")
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

func TestUploadAttachments(t *testing.T) {
	paths := attachmentFiles(t)
	remote := &attachmentMock{dir: "/tmp/agency-attachments.Abc123"}
	var progress []AttachmentProgress
	err := uploadAttachments(context.Background(), remote, "%9", paths, func(p AttachmentProgress) {
		progress = append(progress, p)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(remote.copies) != 2 || len(progress) != 2 || len(remote.sends) != 1 {
		t.Fatalf("copies=%v progress=%v sends=%v", remote.copies, progress, remote.sends)
	}
	for i, copy := range remote.copies {
		if copy[0] != paths[i] || !strings.HasPrefix(copy[1], remote.dir+"/") || strings.ContainsAny(copy[1], " '$();\n") {
			t.Fatalf("unsafe or incorrect copy: %v", copy)
		}
		if progress[i].Index != i+1 || progress[i].Total != 2 || progress[i].Size != 7 {
			t.Fatalf("progress: %+v", progress[i])
		}
		data, err := os.ReadFile(paths[i])
		if err != nil || string(data) != "fixture" {
			t.Fatalf("source changed: %q %v", data, err)
		}
	}
	if remote.copies[0][1] == remote.copies[1][1] {
		t.Fatal("same basenames collided")
	}
	text := " " + remote.copies[0][1] + " " + remote.copies[1][1] + " "
	wantSend := []string{"send", "--no-enter", "%9", text}
	if !reflect.DeepEqual(remote.sends[0], wantSend) || strings.ContainsAny(text, "\r\n") {
		t.Fatalf("send: %v", remote.sends)
	}
	if !strings.Contains(remote.scripts[0], "umask 077") || !strings.Contains(remote.scripts[0], "mktemp -d") || remote.scripts[1] != "chmod 600 -- "+strings.TrimSpace(text) || len(remote.scripts) != 2 {
		t.Fatalf("permissions: %v", remote.scripts)
	}
}

func TestUploadAttachmentsRejectsWholeSelection(t *testing.T) {
	paths := attachmentFiles(t)
	link := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(paths[0], link); err != nil {
		t.Fatal(err)
	}
	for _, selection := range [][]string{nil, {paths[0], t.TempDir()}, {paths[0], paths[0] + ".missing"}, {paths[0], link}} {
		remote := &attachmentMock{}
		if err := uploadAttachments(context.Background(), remote, "%9", selection, func(AttachmentProgress) {}); err == nil {
			t.Fatalf("accepted %v", selection)
		}
		if len(remote.scripts)+len(remote.copies)+len(remote.sends) != 0 {
			t.Fatalf("partial upload: %+v", remote)
		}
	}
}

func TestUploadAttachmentsFailures(t *testing.T) {
	paths := attachmentFiles(t)
	for _, remote := range []*attachmentMock{
		{copyError: 1}, {copyError: 2}, {chmodError: true}, {copyError: 1, cleanError: true},
	} {
		remote.dir = "/tmp/agency-attachments.Abc123"
		err := uploadAttachments(context.Background(), remote, "%9", paths, func(AttachmentProgress) {})
		if err == nil || len(remote.sends) != 0 || remote.scripts[len(remote.scripts)-1] != "rm -rf -- "+remote.dir {
			t.Fatalf("err=%v remote=%+v", err, remote)
		}
		if remote.cleanError && !strings.Contains(err.Error(), "partial uploads remain") {
			t.Fatalf("cleanup failure hidden: %v", err)
		}
	}
}

func TestUploadAttachmentsKeepsFilesOnSendFailure(t *testing.T) {
	remote := &attachmentMock{dir: "/tmp/agency-attachments.Abc123", sendError: true}
	err := uploadAttachments(context.Background(), remote, "%9", attachmentFiles(t), func(AttachmentProgress) {})
	if err == nil || !strings.Contains(err.Error(), "uploaded files remain") || len(remote.scripts) != 2 {
		t.Fatalf("err=%v scripts=%v", err, remote.scripts)
	}
}

func TestUploadAttachmentsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	remote := &attachmentMock{dir: "/tmp/agency-attachments.Abc123"}
	err := uploadAttachments(ctx, remote, "%9", attachmentFiles(t), func(AttachmentProgress) {})
	if !errors.Is(err, context.Canceled) || len(remote.sends) != 0 || remote.scripts[len(remote.scripts)-1] != "rm -rf -- "+remote.dir {
		t.Fatalf("err=%v remote=%+v", err, remote)
	}
}

func TestUploadAttachmentsRejectsUnexpectedDirectory(t *testing.T) {
	for _, dir := range []string{"/", "/tmp", "/tmp/agency-attachments.x/../other", "/tmp/agency-attachments.x;touch bad"} {
		remote := &attachmentMock{dir: dir}
		err := uploadAttachments(context.Background(), remote, "%9", attachmentFiles(t), func(AttachmentProgress) {})
		if err == nil || len(remote.copies)+len(remote.sends) != 0 || len(remote.scripts) != 1 {
			t.Fatalf("dir=%q err=%v remote=%+v", dir, err, remote)
		}
	}
}
