package cloud

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var attachmentDirectory = regexp.MustCompile(`^/tmp/agency-attachments\.[a-zA-Z0-9]+$`)

// AttachmentProgress identifies the file currently uploading.
type AttachmentProgress struct {
	Index int
	Total int
	Name  string
	Size  int64
}

type attachmentRemote interface {
	Shell(context.Context, time.Duration, string) (string, error)
	Copy(context.Context, time.Duration, string, string) error
	Run(context.Context, time.Duration, ...string) (string, error)
}

// UploadAttachments inserts paths without Enter after the whole batch uploads.
// Each batch has a private directory; completed files remain until /tmp cleanup.
func (c *Client) UploadAttachments(ctx context.Context, paneID string, paths []string, progress func(AttachmentProgress)) error {
	return uploadAttachments(ctx, c, paneID, paths, progress)
}

func uploadAttachments(ctx context.Context, remote attachmentRemote, paneID string, paths []string, progress func(AttachmentProgress)) (err error) {
	if len(paths) == 0 {
		return fmt.Errorf("no attachment files selected")
	}
	files := make([]os.FileInfo, len(paths))
	for i, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("attachment %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("attachment %q is not a regular file (directories and symlinks are not uploaded)", path)
		}
		files[i] = info
	}
	dir, err := remote.Shell(ctx, 20*time.Second, "umask 077; mktemp -d /tmp/agency-attachments.XXXXXXXXXX")
	if err != nil {
		return fmt.Errorf("creating private attachment directory: %w", err)
	}
	dir = strings.TrimSpace(dir)
	if !attachmentDirectory.MatchString(dir) {
		return fmt.Errorf("invalid attachment directory returned by Sky")
	}
	complete := false
	defer func() {
		if !complete {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, cleanupErr := remote.Shell(cleanup, 20*time.Second, "rm -rf -- "+dir); cleanupErr != nil {
				err = fmt.Errorf("%w; partial uploads remain in %s: %v", err, dir, cleanupErr)
			}
		}
	}()

	remotePaths := make([]string, len(paths))
	for i, path := range paths {
		name := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, files[i].Name())
		remotePaths[i] = filepath.Join(dir, fmt.Sprintf("%03d-%s", i+1, name))
		progress(AttachmentProgress{Index: i + 1, Total: len(paths), Name: name, Size: files[i].Size()})
		if err := remote.Copy(ctx, 10*time.Minute, path, remotePaths[i]); err != nil {
			return fmt.Errorf("uploading attachment %d/%d %q: %w", i+1, len(paths), path, err)
		}
	}
	if _, err := remote.Shell(ctx, 20*time.Second, "chmod 600 -- "+strings.Join(remotePaths, " ")); err != nil {
		return fmt.Errorf("securing attachments: %w", err)
	}
	complete = true
	if _, err := remote.Run(ctx, 20*time.Second, "send", "--no-enter", paneID, " "+strings.Join(remotePaths, " ")+" "); err != nil {
		return fmt.Errorf("inserting attachment paths (uploaded files remain in %s): %w", dir, err)
	}
	return nil
}
