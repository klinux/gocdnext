package parser

import (
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

const sampleYAML = `
materials:
  - git:
      url: https://github.com/org/repo
      branch: main
      on: [push, pull_request]
      auto_register_webhook: true
  - upstream:
      pipeline: build-core
      stage: test

stages: [build, test, deploy]

variables:
  GO_VERSION: "1.23"

jobs:
  build:
    stage: build
    image: golang:1.23
    script:
      - go build ./...

  test:
    stage: test
    image: golang:1.23
    needs: [build]
    script:
      - go test ./...

  notify:
    stage: deploy
    image: plugins/slack
    settings:
      channel: "#deploys"
`

func TestParse_Happy(t *testing.T) {
	p, err := Parse(strings.NewReader(sampleYAML), "proj-1", "my-pipeline")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(p.Materials) != 2 {
		t.Fatalf("expected 2 materials, got %d", len(p.Materials))
	}
	if p.Materials[0].Type != domain.MaterialGit {
		t.Errorf("material[0] type: want git, got %s", p.Materials[0].Type)
	}
	if !p.Materials[0].Git.AutoRegisterWebhook {
		t.Errorf("auto_register_webhook should be true")
	}
	if p.Materials[1].Upstream.Pipeline != "build-core" {
		t.Errorf("upstream pipeline mismatch")
	}

	if len(p.Stages) != 3 {
		t.Errorf("expected 3 stages, got %d", len(p.Stages))
	}
	if len(p.Jobs) != 3 {
		t.Errorf("expected 3 jobs, got %d", len(p.Jobs))
	}
}

func TestParse_UndeclaredStage(t *testing.T) {
	bad := `
stages: [build]
materials:
  - manual: true
jobs:
  oops:
    stage: nonexistent
    image: alpine
    script: [echo hi]
`
	_, err := Parse(strings.NewReader(bad), "p", "n")
	if err == nil {
		t.Fatal("expected error for undeclared stage")
	}
}

func TestParse_Matrix(t *testing.T) {
	y := `
stages: [test]
materials:
  - manual: true
jobs:
  t:
    stage: test
    image: golang:1.23
    script: [go test ./...]
    parallel:
      matrix:
        - OS: [linux, darwin]
          ARCH: [amd64, arm64]
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	j := p.Jobs[0]
	if len(j.Matrix["OS"]) != 2 || len(j.Matrix["ARCH"]) != 2 {
		t.Errorf("matrix not flattened correctly: %+v", j.Matrix)
	}
}
