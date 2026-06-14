package parser

import (
	"strings"
	"testing"
)

// #42: matrix dimensions decompose into per-job env vars ($OS, $ARCH)
// at dispatch. The parser guards the names/values so the decomposition
// is safe and unambiguous.

func TestParse_Matrix_AcceptsValidDims(t *testing.T) {
	y := `
stages: [test]
jobs:
  build:
    stage: test
    image: golang:1.23
    parallel:
      matrix:
        - OS: [linux, darwin]
          ARCH: [amd64, arm64]
    script: ["go build"]
`
	p, err := ParseNamed(strings.NewReader(y), "p", "ci")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	job := findJobByName(t, p, "build")
	if len(job.Matrix) != 2 {
		t.Fatalf("matrix dims = %d, want 2", len(job.Matrix))
	}
}

func TestParse_Matrix_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "dimension name not a valid env identifier",
			yaml: `
stages: [t]
jobs:
  j:
    stage: t
    image: alpine
    script: ["true"]
    parallel:
      matrix:
        - "build os": [linux]
`,
			wantErr: "valid env var name",
		},
		{
			name: "reserved CI_ prefix",
			yaml: `
stages: [t]
jobs:
  j:
    stage: t
    image: alpine
    script: ["true"]
    parallel:
      matrix:
        - CI_OS: [linux]
`,
			wantErr: "reserved prefix",
		},
		{
			name: "collides with job variable",
			yaml: `
stages: [t]
jobs:
  j:
    stage: t
    image: alpine
    script: ["true"]
    variables:
      OS: override
    parallel:
      matrix:
        - OS: [linux]
`,
			wantErr: "collides",
		},
		{
			name: "collides with secret",
			yaml: `
stages: [t]
jobs:
  j:
    stage: t
    image: alpine
    script: ["true"]
    secrets: [TOKEN]
    parallel:
      matrix:
        - TOKEN: [a, b]
`,
			wantErr: "collides",
		},
		{
			name: "collides with id_token (which overwrites it at dispatch)",
			yaml: `
stages: [t]
jobs:
  j:
    stage: t
    image: alpine
    script: ["true"]
    id_tokens:
      OS:
        aud: https://example.com
    parallel:
      matrix:
        - OS: [linux, darwin]
`,
			wantErr: "collides",
		},
		{
			name: "value contains matrix-key separator",
			yaml: `
stages: [t]
jobs:
  j:
    stage: t
    image: alpine
    script: ["true"]
    parallel:
      matrix:
        - OS: ["linux,extra"]
`,
			wantErr: "',' or '='",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseNamed(strings.NewReader(tt.yaml), "p", "ci")
			if err == nil {
				t.Fatalf("expected rejection for %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
			}
		})
	}
}

func TestParse_Matrix_CollidesWithPipelineVariable(t *testing.T) {
	y := `
name: ci
variables:
  ARCH: x86
stages: [t]
jobs:
  j:
    stage: t
    image: alpine
    script: ["true"]
    parallel:
      matrix:
        - ARCH: [amd64, arm64]
`
	_, err := ParseNamed(strings.NewReader(y), "p", "ci")
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("err = %v, want collision with pipeline variable", err)
	}
}
