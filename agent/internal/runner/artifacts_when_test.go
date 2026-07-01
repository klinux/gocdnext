package runner

import "testing"

func TestShouldUploadArtifacts(t *testing.T) {
	tests := []struct {
		name       string
		when       string
		taskFailed bool
		want       bool
	}{
		{"default success uploads", "", false, true},
		{"default failure skips", "", true, false},
		{"on_success + success uploads", "on_success", false, true},
		{"on_success + failure skips", "on_success", true, false},
		{"on_failure + success skips", "on_failure", false, false},
		{"on_failure + failure uploads", "on_failure", true, true},
		{"always + success uploads", "always", false, true},
		{"always + failure uploads", "always", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldUploadArtifacts(tt.when, tt.taskFailed); got != tt.want {
				t.Fatalf("shouldUploadArtifacts(%q, %v) = %v, want %v", tt.when, tt.taskFailed, got, tt.want)
			}
		})
	}
}
