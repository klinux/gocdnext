package parser

import (
	"strings"
	"testing"
)

func jobYAMLWithArtifactsWhen(when string) string {
	block := ""
	if when != "" {
		block = "\n      when: " + when
	}
	return `
name: ci
stages: [scan]
jobs:
  gitleaks:
    stage: scan
    image: zricethezav/gitleaks
    script: ["gitleaks detect"]
    artifacts:
      paths: [gitleaks.sarif]` + block + `
`
}

func TestParseArtifactsWhen(t *testing.T) {
	tests := []struct {
		name string
		when string
		want string
	}{
		{"unset defaults to on_success", "", ""},
		{"explicit on_success", "on_success", ""},
		{"on_failure", "on_failure", "on_failure"},
		{"always", "always", "always"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Parse(strings.NewReader(jobYAMLWithArtifactsWhen(tt.when)), "proj", "ci")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := p.Jobs[0].ArtifactsWhen; got != tt.want {
				t.Fatalf("ArtifactsWhen = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseArtifactsWhenRejectsUnknown(t *testing.T) {
	// A typo must fail loud, never silently fall back to on_success and hide
	// a red scan's findings from the Security dashboard.
	_, err := Parse(strings.NewReader(jobYAMLWithArtifactsWhen("on_faliure")), "proj", "ci")
	if err == nil {
		t.Fatal("expected error for unknown artifacts.when, got nil")
	}
	if !strings.Contains(err.Error(), "artifacts.when") {
		t.Fatalf("error should name artifacts.when, got: %v", err)
	}
}
