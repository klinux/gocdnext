package parser

import (
	"strings"
	"testing"
)

func TestParseSupersede(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"unset defaults to off", "", ""},
		{"explicit off normalises to empty", "off", ""},
		{"branch", "branch", "branch"},
		{"pipeline", "pipeline", "pipeline"},
		{"case-insensitive", "Branch", "branch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := ""
			if tt.value != "" {
				line = "supersede: " + tt.value + "\n"
			}
			yaml := "name: ci\n" + line + `stages: [deploy]
jobs:
  gate:
    stage: deploy
    approval:
      approvers: [alice]
`
			p, err := Parse(strings.NewReader(yaml), "proj", "ci")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if p.Supersede != tt.want {
				t.Fatalf("Supersede = %q, want %q", p.Supersede, tt.want)
			}
		})
	}
}

func TestParseSupersedeRejectsUnknown(t *testing.T) {
	yaml := `
name: ci
supersede: yolo
stages: [deploy]
jobs:
  gate:
    stage: deploy
    approval:
      approvers: [alice]
`
	if _, err := Parse(strings.NewReader(yaml), "proj", "ci"); err == nil ||
		!strings.Contains(err.Error(), "supersede") {
		t.Fatalf("expected supersede validation error, got %v", err)
	}
}

func TestApprovalRejectsParallelMatrix(t *testing.T) {
	yaml := `
name: ci
stages: [deploy]
jobs:
  gate:
    stage: deploy
    approval:
      approvers: [alice]
    parallel:
      matrix:
        - ENV: [staging, prod]
`
	_, err := Parse(strings.NewReader(yaml), "proj", "ci")
	if err == nil || !strings.Contains(err.Error(), "parallel/matrix") {
		t.Fatalf("expected approval+parallel.matrix rejection, got %v", err)
	}
}
