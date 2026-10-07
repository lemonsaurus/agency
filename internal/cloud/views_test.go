package cloud

import (
	"reflect"
	"strings"
	"testing"
)

func TestShownLineRoundTrips(t *testing.T) {
	for _, keys := range [][]string{{"bluefin-agency/%4", "bluefin-agency/%9"}, nil} {
		device, got, ok := ParseShownLine(ShownLine("bluefin-agency", keys))
		if !ok || device != "bluefin-agency" || len(got) != len(keys) || (len(keys) > 0 && !reflect.DeepEqual(got, keys)) {
			t.Fatalf("round trip of %v = %q %v %v", keys, device, got, ok)
		}
	}
	if _, _, ok := ParseShownLine("changed"); ok {
		t.Fatal("parsed a non-shown line")
	}
}

func TestParkFrameClipsToTheTerminal(t *testing.T) {
	text := "first line of the pane that is far too long to fit\nπ second\n\n\nbottom"
	frame := ParkFrame(text, 30, 3)
	if strings.Contains(frame, "first line") || !strings.Contains(frame, "bottom") {
		t.Fatalf("taller pane did not keep its bottom rows: %q", frame)
	}
	if !strings.Contains(frame, "parked") {
		t.Fatalf("no parked tag: %q", frame)
	}
	wide := ParkFrame("日本語日本語日本語", 6, 1)
	if strings.Contains(wide, "日本語日") {
		t.Fatalf("wide runes overflow 6 cells: %q", wide)
	}
	if ParkFrame("x", 0, 5) != "" {
		t.Fatal("drew into a zero-width terminal")
	}
}
