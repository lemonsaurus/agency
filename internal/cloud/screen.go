package cloud

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

// Screen colors, from the palette Agency's borders use.
const (
	screenMauve = "#cba6f7"
	screenRed   = "#f38ba8"
	screenPeach = "#fab387"
	screenGreen = "#a6e3a1"
	screenBlue  = "#89b4fa"
	screenText  = "#cdd6f4"
	screenDim   = "#7f849c"
	screenFaint = "#45475a"
)

// screenLine is one centered row of the sky screen's card.
type screenLine struct {
	text     string
	color    string
	bold     bool
	emphasis string // follows text in the card's accent, bold
}

// screenCard is the sky screen's content for a link status: what the screen
// says and what Enter does. sky names the host; hint is the empty world's
// key help; note replaces the action while one is under way.
func screenCard(status LinkStatus, sky, hint, note string, now time.Time) (accent string, lines []screenLine) {
	var title, detail, action, emphasis string
	var badge string
	switch {
	case status.State == LinkOffline:
		accent, badge = screenRed, "✕  OFFLINE"
		title, emphasis = "Can't reach ", sky
		detail = fmt.Sprintf("gave up after %d tries", status.Attempts)
		action = "⏎  Enter retries"
	case !status.Here:
		accent, badge = screenMauve, "●  IN USE"
		title, emphasis = "In use on ", status.Lease.Host
		detail = "taken " + Ago(now, status.Lease.Taken)
		if status.Lease.Active > status.Lease.Taken {
			detail += "  ·  active " + Ago(now, status.Lease.Active)
		}
		action = "⏎  Enter takes over here"
		if status.State != LinkUp {
			action = "⏎  Enter reconnects"
		}
	case status.State == LinkUp:
		accent, badge = screenBlue, "☁  SKY"
		title, emphasis = "No remote panes on ", sky
		detail = hint
		action = "⏎  Enter re-syncs"
	default:
		accent, badge = screenPeach, "◌  CONNECTING"
		title, emphasis = "Connecting to ", sky
		detail = "first try"
		if status.Failed > 0 {
			detail = fmt.Sprintf("try %d of %d", status.Failed+1, status.Attempts)
		}
		if status.State == LinkRetrying && status.RetryAt > 0 {
			detail += "  ·  next in " + seconds(time.UnixMilli(status.RetryAt).Sub(now))
		}
		action = "⏎  Enter retries now"
	}
	actionColor := screenGreen
	if note != "" {
		action, actionColor = note, accent
	}
	lines = []screenLine{
		{text: badge, color: accent, bold: true},
		{},
		{text: title, color: screenText, bold: true, emphasis: emphasis},
		{text: detail, color: screenDim},
		{},
		{text: action, color: actionColor, bold: true},
	}
	if status.Error != "" && status.State != LinkUp {
		lines = append(lines, screenLine{}, screenLine{text: status.Error, color: screenFaint})
	}
	return accent, lines
}

// Screen draws the sky screen: the card centered in a rounded box, clipped to
// width × height cells. It clears the whole terminal, so a redraw after a
// resize leaves nothing behind.
func Screen(status LinkStatus, sky, hint, note string, now time.Time, width, height int) string {
	if width < 1 || height < 1 {
		return ""
	}
	accent, lines := screenCard(status, sky, hint, note, now)
	inner := 0
	for _, line := range lines {
		inner = max(inner, runewidth.StringWidth(line.text+line.emphasis))
	}
	inner = min(max(inner+8, 44), width-2)
	var b strings.Builder
	b.WriteString("\x1b[?25l\x1b[0m\x1b[2J")
	boxed := inner >= 12 && height >= len(lines)+4
	top := max(0, (height-len(lines))/2)
	if boxed {
		top = max(1, (height-len(lines)-4)/2+2)
		left := (width-inner-2)/2 + 1
		border := fg(accent) + "\x1b[2m"
		fmt.Fprintf(&b, "\x1b[%d;%dH%s╭%s╮", top-1, left, border, strings.Repeat("─", inner))
		for row := top; row < top+len(lines)+2; row++ {
			fmt.Fprintf(&b, "\x1b[%d;%dH%s│\x1b[%d;%dH│", row, left, border, row, left+inner+1)
		}
		fmt.Fprintf(&b, "\x1b[%d;%dH╰%s╯\x1b[0m", top+len(lines)+2, left, strings.Repeat("─", inner))
		top++
	}
	room := width
	if boxed {
		room = inner - 2
	}
	for i, line := range lines {
		if line.text == "" || top+i > height {
			continue
		}
		text := clip(line.text, room)
		emphasis := clip(line.emphasis, room-runewidth.StringWidth(text))
		column := (width-runewidth.StringWidth(text+emphasis))/2 + 1
		style := fg(line.color)
		if line.bold {
			style += "\x1b[1m"
		}
		fmt.Fprintf(&b, "\x1b[%d;%dH%s%s\x1b[0m%s\x1b[1m%s\x1b[0m", top+i, column, style, text, fg(accent), emphasis)
	}
	return b.String()
}

// Ago says how long before now the Unix ms at was, the way a person would.
func Ago(now time.Time, at int64) string {
	if at <= 0 {
		return "a while ago"
	}
	d := now.Sub(time.UnixMilli(at))
	switch {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h ago"
	}
	return strconv.Itoa(int(d.Hours()/24)) + " days ago"
}

func seconds(d time.Duration) string {
	return strconv.Itoa(max(0, int(d.Round(time.Second).Seconds()))) + "s"
}

// fg is the truecolor escape for a "#rrggbb" color.
func fg(hex string) string {
	value, err := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", value>>16, value>>8&0xff, value&0xff)
}
