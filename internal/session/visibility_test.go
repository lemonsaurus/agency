package session

import (
	"reflect"
	"testing"
)

func TestVisibleViewers(t *testing.T) {
	out := "%1\t@9\t1\t0\t0\n" + // seen
		"%2\t@10\t0\t0\t1\n" + // window not current anywhere
		"%3\t@11\t1\t1\t0\n" + // zoomed away
		"%4\t@12\t2\t1\t1\n" + // zoomed, active
		"%5\tnone\t1\t0\t1\n" + // placeholder
		"%6\t\t1\t0\t1\n" // not a viewer
	got := visibleViewers("bluefin-agency", out)
	want := []string{"bluefin-agency/%1", "bluefin-agency/%4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("visible = %v, want %v", got, want)
	}
}
