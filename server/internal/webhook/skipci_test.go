package webhook

import "testing"

func TestSkipCIMarker(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
		found   bool
	}{
		{"skip ci lowercase", "chore: bump tags [skip ci]", "[skip ci]", true},
		{"ci skip variant", "[ci skip] release housekeeping", "[ci skip]", true},
		{"no ci variant", "docs: typo [no ci]", "[no ci]", true},
		{"uppercase", "chore [SKIP CI] bump", "[skip ci]", true},
		{"mixed case", "chore [Skip Ci] bump", "[skip ci]", true},
		{"marker in body not title", "feat: thing\n\nlong body\n[skip ci]", "[skip ci]", true},
		{"no marker", "feat: add skip ci support", "", false},
		{"words without brackets", "please skip ci on this one", "", false},
		{"half bracket", "[skip ci missing close", "", false},
		{"empty message", "", "", false},
		{"product-specific marker not honored", "[skip actions] gha-only", "", false},
		{"unicode around marker", "über-fix ✨ [skip ci] ümlauts", "[skip ci]", true},
		{"first match wins for logging", "[ci skip] and also [skip ci]", "[skip ci]", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := skipCIMarker(tt.message)
			if found != tt.found || got != tt.want {
				t.Fatalf("skipCIMarker(%q) = (%q, %v), want (%q, %v)",
					tt.message, got, found, tt.want, tt.found)
			}
		})
	}
}
