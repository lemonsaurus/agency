package restore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lemonsaurus/agency/internal/ipc"
)

// bridgeWait is how often to look for a Pi bridge that has not come up yet.
const bridgeWait = time.Second

// BridgeNotifier delivers a note through the pane's Pi bridge, waiting for a
// freshly started Pi to open it. daemonSocket is the daemon's socket path.
func BridgeNotifier(daemonSocket string) func(ctx context.Context, id, text string) error {
	return func(ctx context.Context, id, text string) error {
		path, err := ipc.BridgePath(daemonSocket, id)
		if err != nil {
			return err
		}
		for {
			if _, err := os.Stat(path); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("Pi in %s never opened its bridge", id)
			case <-time.After(bridgeWait):
			}
		}
		request := make([]byte, 16)
		rand.Read(request)
		err = ipc.Send(ctx, path, ipc.SendRequest{ID: hex.EncodeToString(request), Text: text})
		var refusal *ipc.BridgeRefusal
		if errors.Is(err, ipc.ErrNoBridge) || errors.As(err, &refusal) {
			return err
		}
		return nil
	}
}
