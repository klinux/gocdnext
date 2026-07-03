package store

import (
	"encoding/json"
	"testing"
)

func TestPRLaneRef(t *testing.T) {
	tests := []struct {
		name   string
		in     any
		want   string
		wantOK bool
	}{
		{"json float", float64(5), "pr:5", true},
		{"decimal string", "5", "pr:5", true},
		{"json.Number", json.Number("42"), "pr:42", true},
		{"zero rejected", float64(0), "", false},
		{"negative rejected", float64(-1), "", false},
		{"non-integer float rejected", float64(5.5), "", false},
		{"decimal-looking string rejected", "5.0", "", false},
		{"non-numeric string rejected", "abc", "", false},
		{"nil rejected", nil, "", false},
		{"bool rejected", true, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := prLaneRef(tt.in)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("prLaneRef(%#v) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
