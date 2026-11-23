package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// leaseOption names the one device whose viewers stream: "<device> <host>
// <taken, Unix ms>". Unset until a device first connects.
const leaseOption = "@agency_lease"

// Lease is the host's record of the device that holds the sky harness. Only
// its keyed viewers may attach; keyless clients such as the phone are exempt.
type Lease struct {
	Device string `json:"device,omitempty"`
	Host   string `json:"host,omitempty"`   // the device's hostname, for people
	Taken  int64  `json:"taken,omitempty"`  // Unix ms the device took the lease
	Active int64  `json:"active,omitempty"` // Unix ms a terminal of the device last took focus
}

// Holds reports whether the lease lets device's viewers attach: it names the
// device, or nobody holds it yet.
func (l Lease) Holds(device string) bool {
	return l.Device == "" || l.Device == device
}

func formatLease(device, host string, taken int64) string {
	return fmt.Sprintf("%s %s %d", device, host, taken)
}

func parseLease(value string) Lease {
	fields := strings.Fields(value)
	if len(fields) != 3 {
		return Lease{}
	}
	taken, _ := strconv.ParseInt(fields[2], 10, 64)
	return Lease{Device: fields[0], Host: fields[1], Taken: taken}
}

// Lease reads who holds the lease and when that device last took focus. A
// failed read is an error, never an unheld lease.
func (c *Client) Lease(ctx context.Context) (Lease, error) {
	value, err := c.Cmd.Run(ctx, "show-options", "-gqv", leaseOption)
	if err != nil {
		return Lease{}, err
	}
	lease := parseLease(value)
	if lease.Device != "" {
		lease.Active, _ = strconv.ParseInt(c.showOption(ctx, focusedOption(lease.Device)), 10, 64)
	}
	return lease, nil
}

// ClaimLease gives device the lease only while nobody holds it. tmux runs
// the check and the set as one command, so two devices that connect together
// cannot both win.
func (c *Client) ClaimLease(ctx context.Context, device, host string, now time.Time) error {
	value := formatLease(device, host, now.UnixMilli())
	_, err := c.Cmd.Run(ctx, "if-shell", "-F", "#{==:#{"+leaseOption+"},}", "set-option -g "+leaseOption+" '"+value+"'")
	if err != nil {
		return err
	}
	return c.dropOtherViewers(ctx)
}

// TakeLease moves the lease to device and detaches every other device's
// viewers, so the previous holder keeps no attach clients.
func (c *Client) TakeLease(ctx context.Context, device, host string, now time.Time) error {
	if _, err := c.Cmd.Run(ctx, "set-option", "-g", leaseOption, formatLease(device, host, now.UnixMilli())); err != nil {
		return err
	}
	return c.dropOtherViewers(ctx)
}

// dropOtherViewers kills the view and park sessions of keyed viewers whose
// device does not hold the lease. Their clients detach and their SSH
// attachments end.
func (c *Client) dropOtherViewers(ctx context.Context) error {
	lease := parseLease(c.showOption(ctx, leaseOption))
	sessions, err := c.viewSessions(ctx)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		device, keyed := viewerDevice(s.key)
		if !keyed || !strings.HasPrefix(s.name, ViewPrefix) || lease.Holds(device) {
			continue
		}
		park := ParkPrefix + strings.TrimPrefix(s.name, ViewPrefix)
		_, _ = c.Cmd.Run(ctx, "kill-session", "-t", "="+park)
		_, _ = c.Cmd.Run(ctx, "kill-session", "-t", "="+s.name)
	}
	return nil
}
