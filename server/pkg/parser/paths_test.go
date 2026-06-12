package parser

import (
	"strings"
	"testing"
)

// when.paths lowers into Pipeline.TriggerPaths and must arrive
// validated: every glob compiles, workspace-relative, no traversal.
func TestParseWhenPaths(t *testing.T) {
	yaml := `
name: ci
when:
  event: [push, pull_request]
  paths:
    - "**/*.go"
    - "go.mod"
    - "cmd/**"
stages: [test]
jobs:
  unit:
    stage: test
    image: golang:1.26
    script: ["go test ./..."]
`
	p, err := Parse(strings.NewReader(yaml), "proj", "ci")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"**/*.go", "go.mod", "cmd/**"}
	if len(p.TriggerPaths) != len(want) {
		t.Fatalf("TriggerPaths = %v, want %v", p.TriggerPaths, want)
	}
	for i := range want {
		if p.TriggerPaths[i] != want[i] {
			t.Fatalf("TriggerPaths[%d] = %q, want %q", i, p.TriggerPaths[i], want[i])
		}
	}
}

func TestParseWhenPathsRejections(t *testing.T) {
	tests := []struct {
		name    string
		paths   string
		wantErr string
	}{
		{"invalid glob", `["[unclosed"]`, "when.paths"},
		{"traversal", `["../secrets/**"]`, "when.paths"},
		{"absolute", `["/etc/passwd"]`, "when.paths"},
		{"empty entry", `["web/**", ""]`, "when.paths"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := `
name: ci
when:
  paths: ` + tt.paths + `
stages: [test]
jobs:
  unit:
    stage: test
    image: golang:1.26
    script: ["go test ./..."]
`
			_, err := Parse(strings.NewReader(yaml), "proj", "ci")
			if err == nil {
				t.Fatalf("expected error for paths %s", tt.paths)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
			}
		})
	}
}
