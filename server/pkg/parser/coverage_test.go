package parser

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func TestParseCoverageReport(t *testing.T) {
	yaml := `
name: ci
stages: [test]
jobs:
  unit:
    stage: test
    image: golang:1.26
    script: ["go test -coverprofile=coverage.out ./..."]
    coverage_report:
      path: coverage.out
      format: go-cover
`
	p, err := Parse(strings.NewReader(yaml), "proj", "ci")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cr := p.Jobs[0].CoverageReport
	if cr == nil {
		t.Fatal("CoverageReport = nil")
	}
	if cr.Path != "coverage.out" || cr.Format != "go-cover" {
		t.Fatalf("CoverageReport = %+v", cr)
	}
}

func TestParseCoverageReportRejections(t *testing.T) {
	tests := []struct {
		name    string
		block   string
		wantErr string
	}{
		{"missing path", "format: go-cover", "coverage_report.path"},
		{"missing format", `path: cover.out`, "coverage_report.format"},
		{"unknown format", "path: c.out\n      format: gcov", "coverage_report.format"},
		{"absolute path", "path: /etc/passwd\n      format: lcov", "coverage_report.path"},
		{"traversal", "path: ../../c.out\n      format: lcov", "coverage_report.path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := `
name: ci
stages: [test]
jobs:
  unit:
    stage: test
    image: golang:1.26
    script: ["true"]
    coverage_report:
      ` + tt.block + `
`
			_, err := Parse(strings.NewReader(yaml), "proj", "ci")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseCoverageReportRoundTripsJSON(t *testing.T) {
	yaml := `
name: ci
stages: [test]
jobs:
  unit:
    stage: test
    image: golang:1.26
    script: ["true"]
    coverage_report: {path: lcov.info, format: lcov}
`
	p, err := Parse(strings.NewReader(yaml), "proj", "ci")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The definition persists as JSONB — the spec must survive the
	// trip (same guard every persisted job field carries).
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back domain.Pipeline
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cr := back.Jobs[0].CoverageReport
	if cr == nil || cr.Path != "lcov.info" || cr.Format != "lcov" {
		t.Fatalf("round-tripped CoverageReport = %+v", cr)
	}
}
