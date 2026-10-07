package tmux

import (
	"reflect"
	"testing"
)

func TestShownOptionAndParse(t *testing.T) {
	if got := shownOption("blue-fin.agency"); got != "@agency_shown_blue_fin_agency" {
		t.Fatalf("option name = %q", got)
	}
	if keys, set := parseShown(""); set || keys != nil {
		t.Fatal("unset option parsed as a set")
	}
	if keys, set := parseShown("="); !set || len(keys) != 0 {
		t.Fatalf("empty set = %v %v", keys, set)
	}
	if keys, _ := parseShown("=a/%1 a/%2"); !reflect.DeepEqual(keys, []string{"a/%1", "a/%2"}) {
		t.Fatalf("keys = %v", keys)
	}
}

func TestViewerDeviceAndPID(t *testing.T) {
	if device, ok := viewerDevice("bluefin-agency/%42"); !ok || device != "bluefin-agency" {
		t.Fatalf("device = %q %v", device, ok)
	}
	if _, ok := viewerDevice(""); ok {
		t.Fatal("empty key has a device")
	}
	for name, want := range map[string]int{"view-588851": 588851, "park-12": 12} {
		if pid, ok := viewPID(name); !ok || pid != want {
			t.Fatalf("viewPID(%q) = %d %v", name, pid, ok)
		}
	}
	for _, name := range []string{"cloud", "view-main", "remote-sky"} {
		if _, ok := viewPID(name); ok {
			t.Fatalf("%q parsed as a view session", name)
		}
	}
}
