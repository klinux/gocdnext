package parser

import (
	"strings"
	"testing"
)

func TestParse_ExtendsMergesScalarsAndLists(t *testing.T) {
	y := `
stages: [test]
materials: [{manual: true}]
jobs:
  .base-go:
    stage: test
    image: golang:1.23
    script: [make test]
    secrets: [CODECOV_TOKEN]
  unit:
    extends: .base-go
  # Child overrides image + script but keeps secrets from base.
  unit-race:
    extends: .base-go
    image: golang:1.24
    script: [make test-race]
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2 (template dropped)", len(p.Jobs))
	}
	byName := map[string]int{}
	for i, j := range p.Jobs {
		byName[j.Name] = i
	}
	if _, ok := byName[".base-go"]; ok {
		t.Fatal("hidden template leaked into materialized jobs")
	}

	unit := p.Jobs[byName["unit"]]
	if unit.Image != "golang:1.23" {
		t.Errorf("unit image = %q, want golang:1.23 (inherited)", unit.Image)
	}
	if got := unit.Tasks; len(got) != 1 || got[0].Script == "" || !strings.Contains(got[0].Script, "make test") {
		t.Errorf("unit tasks = %+v, want single script task containing 'make test'", got)
	}
	if got := unit.Secrets; len(got) != 1 || got[0] != "CODECOV_TOKEN" {
		t.Errorf("unit secrets = %+v, want [CODECOV_TOKEN] (inherited)", got)
	}

	race := p.Jobs[byName["unit-race"]]
	if race.Image != "golang:1.24" {
		t.Errorf("race image = %q, want golang:1.24 (overridden)", race.Image)
	}
	if got := race.Tasks; len(got) != 1 || !strings.Contains(got[0].Script, "make test-race") {
		t.Errorf("race tasks = %+v, want script to be the child's", got)
	}
	if got := race.Secrets; len(got) != 1 || got[0] != "CODECOV_TOKEN" {
		t.Errorf("race secrets = %+v, want inherited from base", got)
	}
}

func TestParse_ExtendsOverlaysMaps(t *testing.T) {
	y := `
stages: [build]
materials: [{manual: true}]
jobs:
  .base:
    stage: build
    image: node:20
    variables:
      FOO: from-base
      KEEP: still-here
  pkg:
    extends: .base
    variables:
      FOO: from-child
      EXTRA: only-in-child
    script: [pnpm build]
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(p.Jobs))
	}
	vars := p.Jobs[0].Variables
	if vars["FOO"] != "from-child" {
		t.Errorf("FOO = %q, want child's", vars["FOO"])
	}
	if vars["KEEP"] != "still-here" {
		t.Errorf("KEEP = %q, want inherited", vars["KEEP"])
	}
	if vars["EXTRA"] != "only-in-child" {
		t.Errorf("EXTRA = %q, want child's", vars["EXTRA"])
	}
}

func TestParse_ExtendsChainedDeeply(t *testing.T) {
	y := `
stages: [test]
materials: [{manual: true}]
jobs:
  .a:
    stage: test
    image: alpine
    secrets: [A_TOKEN]
  .b:
    extends: .a
    secrets: [B_TOKEN]
  .c:
    extends: .b
    script: [echo c]
  real:
    extends: .c
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1 (only real should materialize)", len(p.Jobs))
	}
	real := p.Jobs[0]
	if real.Image != "alpine" {
		t.Errorf("image didn't chain through: %q", real.Image)
	}
	if got := real.Secrets; len(got) != 1 || got[0] != "B_TOKEN" {
		t.Errorf("secrets = %+v, want B_TOKEN (child list REPLACES parent list)", got)
	}
	if got := real.Tasks; len(got) != 1 || !strings.Contains(got[0].Script, "echo c") {
		t.Errorf("script didn't chain through: %+v", got)
	}
}

func TestParse_ExtendsCycleFails(t *testing.T) {
	y := `
stages: [test]
materials: [{manual: true}]
jobs:
  a:
    stage: test
    image: alpine
    extends: b
  b:
    stage: test
    image: alpine
    extends: a
`
	_, err := Parse(strings.NewReader(y), "p", "n")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestParse_ExtendsUnknownTargetFails(t *testing.T) {
	y := `
stages: [build]
materials: [{manual: true}]
jobs:
  one:
    stage: build
    image: alpine
    extends: .does-not-exist
`
	_, err := Parse(strings.NewReader(y), "p", "n")
	if err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("want not-defined error, got %v", err)
	}
}
