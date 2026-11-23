package cloud

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mattn/go-runewidth"
)

// Device names one local Agency session to the host. Viewer keys and the
// host's shown sets are scoped by it, so every machine virtualizes its own
// mirrors independently.
func Device(session string) string {
	return Hostname() + "-" + session
}

// Hostname is this machine's short hostname, as the lease shows it.
func Hostname() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "local"
	}
	host, _, _ = strings.Cut(host, ".")
	return host
}

// LeaseRequest asks the host for the lease: "claim" takes it only while
// nobody holds it, "take" moves it from whoever does.
func LeaseRequest(verb, device, host string) string {
	return verb + " " + device + " " + host
}

// ParseLeaseRequest reads a LeaseRequest.
func ParseLeaseRequest(line string) (verb, device, host string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != "claim" && fields[0] != "take" {
		return "", "", "", false
	}
	return fields[0], fields[1], fields[2], true
}

// ViewerKey identifies one local viewer pane to the host.
func ViewerKey(device, pane string) string {
	return device + "/" + pane
}

// ShownLine tells the host which of a device's viewers are on screen and when
// a terminal of the device last took focus, in Unix milliseconds, 0 for never.
// An empty set is a valid line: the device shows no viewer.
func ShownLine(device string, focused int64, keys []string) string {
	return strings.TrimSpace(fmt.Sprintf("shown %s %d %s", device, focused, strings.Join(keys, " ")))
}

// ParseShownLine reads a ShownLine.
func ParseShownLine(line string) (device string, focused int64, keys []string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "shown" {
		return "", 0, nil, false
	}
	focused, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return "", 0, nil, false
	}
	return fields[1], focused, fields[3:], true
}

const (
	parkStyle = "\x1b[2m\x1b[38;5;246m"
	parkTag   = " parked "
)

// parkEmpty stands in for a snapshot the host could not capture.
const parkEmpty = "no snapshot yet"

// ParkFrame draws what a parked viewer shows: the pane's last text, grey,
// clipped to width × height cells with a "parked" tag in the top right. A
// taller pane loses its top rows, since an agent's input sits at the bottom.
// Without text the frame says so in its middle row.
func ParkFrame(text string, width, height int) string {
	if width < 1 || height < 1 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if strings.TrimSpace(text) == "" {
		lines = make([]string, height/2+1)
		lines[height/2] = strings.Repeat(" ", max(0, (width-len(parkEmpty))/2)) + parkEmpty
	}
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	var b strings.Builder
	b.WriteString("\x1b[?25l\x1b[2J")
	for i, line := range lines {
		room := width
		tagged := i == 0 && width > 2*len(parkTag)
		if tagged {
			room = width - len(parkTag) - 1
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s%s\x1b[0m", i+1, parkStyle, clip(line, room))
	}
	if len(lines) == 0 || width <= 2*len(parkTag) {
		return b.String()
	}
	fmt.Fprintf(&b, "\x1b[1;%dH\x1b[7m%s%s\x1b[0m", width-len(parkTag)+1, parkStyle, parkTag)
	return b.String()
}

// clip cuts line to width cells, replacing control characters with spaces.
func clip(line string, width int) string {
	var b strings.Builder
	used := 0
	for _, r := range line {
		if r < ' ' || r == 0x7f {
			r = ' '
		}
		w := runewidth.RuneWidth(r)
		if used+w > width {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String()
}
