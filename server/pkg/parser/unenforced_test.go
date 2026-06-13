package parser

import (
	"strings"
	"testing"
)

// Issue #40: keys the parser ACCEPTED but nothing enforced were a
// broken promise (the migrations train almost shipped `rules:` as a
// safety rail before review caught that it gates nothing). Until
// they are implemented, declaring them is a parse error pointing at
// the working alternatives.
func TestParseRejectsUnenforcedKeys(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "job rules",
			yaml: `
name: ci
stages: [t]
jobs:
  j:
    stage: t
    image: alpine
    script: ["true"]
    rules:
      - if: "$CI_COMMIT_BRANCH == main"
        when: on_success
`,
			wantErr: "rules:",
		},
		{
			name: "pipeline when.status",
			yaml: `
name: ci
when:
  event: [push]
  status: [success]
stages: [t]
jobs:
  j: {stage: t, image: alpine, script: ["true"]}
`,
			wantErr: "when.status",
		},
		{
			name: "job-level when",
			yaml: `
name: ci
stages: [t]
jobs:
  j:
    stage: t
    image: alpine
    script: ["true"]
    when:
      event: [push]
`,
			wantErr: "when:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.yaml), "proj", "ci")
			if err == nil {
				t.Fatalf("expected rejection for %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), "not enforced") && !strings.Contains(err.Error(), "not implemented") {
				t.Fatalf("err should say WHY (not enforced/implemented): %v", err)
			}
		})
	}
}
