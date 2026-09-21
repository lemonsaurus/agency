package control

import (
	"strings"
	"testing"
)

func TestValidateTaskLabel(t *testing.T) {
	for _, tt := range []struct {
		name     string
		label    string
		required bool
		wantErr  bool
	}{
		{"task", "Cloud Harness Setup", true, false},
		{"unicode limit", strings.Repeat("界", 100), true, false},
		{"unicode over limit", strings.Repeat("界", 101), true, true},
		{"missing", "", true, true},
		{"blank", " \u2003 ", true, true},
		{"clear", "", false, false},
		{"newline", "one\ntwo", false, true},
		{"carriage return", "one\rtwo", false, true},
		{"tab", "one\ttwo", false, true},
		{"escape", "\x1b[31m", false, true},
		{"null", "one\x00two", false, true},
		{"delete", "one\x7f", false, true},
		{"next line", "one\u0085two", false, true},
		{"line separator", "one\u2028two", false, true},
		{"paragraph separator", "one\u2029two", false, true},
		{"joined emoji", "Cloud 👩‍💻", true, false},
		{"invalid utf8", "\xff", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateTaskLabel(tt.label, tt.required); (err != nil) != tt.wantErr {
				t.Fatalf("ValidateTaskLabel(%q) = %v", tt.label, err)
			}
		})
	}
}

func TestCanLabelPane(t *testing.T) {
	for _, tt := range []struct {
		requester Requester
		target    string
		allowed   bool
	}{
		{Requester{Role: RoleWorker, PaneID: "%2"}, "%2", true},
		{Requester{Role: RoleWorker, PaneID: "%2"}, "%3", false},
		{Requester{Role: RoleManager, PaneID: "%1"}, "%3", true},
		{Requester{Role: RoleController, PaneID: "%0"}, "%3", true},
		{Requester{Human: true}, "%3", true},
		{Requester{}, "%3", false},
	} {
		if got := tt.requester.CanLabelPane(tt.target); got != tt.allowed {
			t.Errorf("%+v labels %s: got %v", tt.requester, tt.target, got)
		}
	}
}
