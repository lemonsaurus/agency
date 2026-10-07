package cloud

import (
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-runewidth"
)

// Device names one local Agency session to the host. Viewer keys and the
// host's shown sets are scoped by it, so every machine virtualizes its own
// mirrors independently.
func Device(session string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "local"
	}
	host, _, _ = strings.Cut(host, ".")
	return host + "-" + session
}

// ViewerKey identifies one local viewer pane to the host.
func ViewerKey(device, pane string) string {
	return device + "/" + pane
}

// ShownLine tells the host which of a device's viewers are on screen. An
// empty set is a valid line: the device shows no viewer.
func ShownLine(device string, keys []string) string {
	return strings.TrimSpace("shown " + device + " " + strings.Join(keys, " "))
}

// ParseShownLine reads a ShownLine.
func ParseShownLine(line string) (device string, keys []string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "shown" {
		return "", nil, false
	}
	return fields[1], fields[2:], true
}

const (
	parkStyle = "\x1b[2m\x1b[38;5;246m"
	parkTag   = " parked "
)

// ParkFrame draws what a parked viewer shows: the pane's last text, grey,
// clipped to width × height cells with a "parked" tag in the top right. A
// taller pane loses its top rows, since an agent's input sits at the bottom.
func ParkFrame(text string, width, height int) string {
	if width < 1 || height < 1 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
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
