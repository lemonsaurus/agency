package tmux

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseLease(t *testing.T) {
	lease := parseLease(formatLease("bluefin-agency", "bluefin", 1791448552000))
	if lease != (Lease{Device: "bluefin-agency", Host: "bluefin", Taken: 1791448552000}) {
		t.Fatalf("lease = %+v", lease)
	}
	if parseLease("") != (Lease{}) || parseLease("half written") != (Lease{}) {
		t.Fatal("parsed a malformed lease")
	}
	if !(Lease{}).Holds("anyone") || !lease.Holds("bluefin-agency") || lease.Holds("CITRUS-BRUTE-agency") {
		t.Fatal("Holds")
	}
}

func TestAttachWindowRefusesADeviceWithoutTheLease(t *testing.T) {
	mock := NewMockCommander()
	mock.On("bluefin-agency bluefin 1", nil, "show-options", "-gqv", leaseOption)
	c := &Client{Cmd: mock, SessionName: "cloud"}
	err := c.AttachWindow(context.Background(), "@3", "CITRUS-BRUTE-agency/%7")
	if err == nil || !strings.Contains(err.Error(), "in use on bluefin") {
		t.Fatalf("err = %v", err)
	}
	for _, call := range mock.Calls {
		if call[0] == "new-session" || call[0] == "attach-session" {
			t.Fatalf("refused viewer still ran %v", call)
		}
	}
}

func TestTakeLeaseDropsOtherDevicesViewers(t *testing.T) {
	mock := NewMockCommander()
	mock.On("bluefin-agency bluefin 5", nil, "show-options", "-gqv", leaseOption)
	mock.On("cloud\t\t\t3\n"+
		"view-11\tbluefin-agency/%1\t@3\t1\n"+
		"view-12\tCITRUS-BRUTE-agency/%4\t@3\t0\n"+
		"park-12\t\t\t1\n"+
		"view-13\t\t@3\t1",
		nil, "list-sessions", "-F", "#{session_name}\t#{@agency_viewer}\t#{@agency_window}\t#{session_attached}")
	c := &Client{Cmd: mock, SessionName: "cloud"}
	if err := c.TakeLease(context.Background(), "bluefin-agency", "bluefin", time.UnixMilli(5)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(mock.Calls[0], " "); got != "set-option -g @agency_lease bluefin-agency bluefin 5" {
		t.Fatalf("set = %q", got)
	}
	var killed []string
	for _, call := range mock.Calls {
		if call[0] == "kill-session" {
			killed = append(killed, call[2])
		}
	}
	if strings.Join(killed, " ") != "=park-12 =view-12" {
		t.Fatalf("killed %v, want only the other device's view and park", killed)
	}
}

func TestClaimLeaseSetsOnlyAnUnheldLease(t *testing.T) {
	mock := NewMockCommander()
	c := &Client{Cmd: mock, SessionName: "cloud"}
	if err := c.ClaimLease(context.Background(), "bluefin-agency", "bluefin", time.UnixMilli(5)); err != nil {
		t.Fatal(err)
	}
	want := []string{"if-shell", "-F", "#{==:#{@agency_lease},}", "set-option -g @agency_lease 'bluefin-agency bluefin 5'"}
	if strings.Join(mock.Calls[0], "|") != strings.Join(want, "|") {
		t.Fatalf("claim = %q", mock.Calls[0])
	}
}
