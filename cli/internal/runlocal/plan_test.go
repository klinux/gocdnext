package runlocal

import (
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/pkg/parser"
)

func mustParse(t *testing.T, yaml string) *Plan {
	t.Helper()
	p, err := parser.Parse(strings.NewReader(yaml), "proj", "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := Build(p, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return plan
}

func TestBuild_StageAndNeedsOrder(t *testing.T) {
	plan := mustParse(t, `
name: ci
stages: [test, build]
jobs:
  b:
    stage: test
    needs: [a]
    image: alpine:3.20
    script: ["true"]
  a:
    stage: test
    image: alpine:3.20
    script: ["true"]
  pack:
    stage: build
    image: alpine:3.20
    script: ["true"]
`)
	if len(plan.Stages) != 2 {
		t.Fatalf("stages = %d, want 2", len(plan.Stages))
	}
	test := plan.Stages[0]
	if test.Jobs[0].Name != "a" || test.Jobs[1].Name != "b" {
		t.Fatalf("needs order broken: %s then %s", test.Jobs[0].Name, test.Jobs[1].Name)
	}
	if plan.Stages[1].Jobs[0].Name != "pack" {
		t.Fatalf("stage 2 = %s, want pack", plan.Stages[1].Jobs[0].Name)
	}
}

func TestBuild_MatrixExpansion(t *testing.T) {
	plan := mustParse(t, `
name: ci
stages: [test]
jobs:
  unit:
    stage: test
    image: alpine:3.20
    script: ["echo $GO"]
    parallel:
      matrix:
        - GO: ["1.24", "1.25"]
          OS: ["alpine"]
`)
	jobs := plan.Stages[0].Jobs
	if len(jobs) != 2 {
		t.Fatalf("cells = %d, want 2", len(jobs))
	}
	// Dispatch parity: dims live in GOCDNEXT_MATRIX, never as
	// individual env vars (a local-only $GO would green-light
	// scripts that break in the cluster).
	if jobs[0].MatrixKey != "GO=1.24,OS=alpine" || jobs[1].MatrixKey != "GO=1.25,OS=alpine" {
		t.Fatalf("matrix keys wrong: %q / %q", jobs[0].MatrixKey, jobs[1].MatrixKey)
	}
	if _, leaked := jobs[0].Variables["GO"]; leaked {
		t.Fatalf("matrix dim leaked into Variables: %v", jobs[0].Variables)
	}
	if !strings.Contains(jobs[0].Name, "GO=1.24") {
		t.Fatalf("cell name missing dims: %s", jobs[0].Name)
	}
}

func TestBuild_PluginJobEnv(t *testing.T) {
	plan := mustParse(t, `
name: ci
stages: [build]
jobs:
  image:
    stage: build
    uses: ghcr.io/klinux/gocdnext-plugin-buildx@v1
    with:
      image: ghcr.io/acme/shop
      build-args: "A=1"
`)
	j := plan.Stages[0].Jobs[0]
	if j.Image != "ghcr.io/klinux/gocdnext-plugin-buildx:v1" {
		t.Fatalf("plugin image = %s", j.Image)
	}
	if j.PluginEnv["PLUGIN_IMAGE"] != "ghcr.io/acme/shop" {
		t.Fatalf("PLUGIN_IMAGE = %q", j.PluginEnv["PLUGIN_IMAGE"])
	}
	if j.PluginEnv["PLUGIN_BUILD_ARGS"] != "A=1" {
		t.Fatalf("PLUGIN_BUILD_ARGS = %q (kebab→snake broken)", j.PluginEnv["PLUGIN_BUILD_ARGS"])
	}
}

func TestBuild_ApprovalFlagged(t *testing.T) {
	plan := mustParse(t, `
name: rel
stages: [promote]
jobs:
  gate:
    stage: promote
    approval:
      description: "go?"
  ship:
    stage: promote
    needs: [gate]
    image: alpine:3.20
    script: ["true"]
`)
	jobs := plan.Stages[0].Jobs
	if !jobs[0].Approval || jobs[0].Name != "gate" {
		t.Fatalf("approval job not flagged first: %+v", jobs[0])
	}
	if jobs[1].Approval {
		t.Fatalf("ship wrongly flagged as approval")
	}
}

// Review-round MEDIUM: id_tokens only exist in the cluster — a job
// declaring them must fail the plan loudly, never run token-less.
func TestBuild_IDTokensFailLoud(t *testing.T) {
	p, err := parser.Parse(strings.NewReader(`
name: deploy
stages: [ship]
jobs:
  push:
    stage: ship
    image: alpine:3.20
    script: ["true"]
    id_tokens:
      GCP_TOKEN:
        aud: https://example.com
`), "proj", "deploy")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Build(p, ""); err == nil || !strings.Contains(err.Error(), "id_tokens") {
		t.Fatalf("Build = %v, want id_tokens fail-loud", err)
	}
}

// Review-round MEDIUM: matrix cells keep the YAML name in BaseName
// so --job targets all cells.
func TestBuild_MatrixBaseName(t *testing.T) {
	plan := mustParse(t, `
name: ci
stages: [test]
jobs:
  unit:
    stage: test
    image: alpine:3.20
    script: ["true"]
    parallel:
      matrix:
        - GO: ["1.24", "1.25"]
`)
	for _, j := range plan.Stages[0].Jobs {
		if j.BaseName != "unit" {
			t.Fatalf("BaseName = %q, want unit", j.BaseName)
		}
	}
}

// Review-round MEDIUM: --job must skip non-selected jobs BEFORE the
// unsupported-feature checks — a deploy job with id_tokens cannot
// block `--job lint`.
func TestBuild_OnlyJobSkipsUnsupportedSiblings(t *testing.T) {
	p, err := parser.Parse(strings.NewReader(`
name: ci
stages: [test, ship]
jobs:
  lint:
    stage: test
    image: alpine:3.20
    script: ["true"]
  deploy:
    stage: ship
    image: alpine:3.20
    script: ["true"]
    id_tokens:
      GCP_TOKEN:
        aud: https://example.com
`), "proj", "ci")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := Build(p, "lint")
	if err != nil {
		t.Fatalf("Build(only=lint) = %v — sibling id_tokens must not block", err)
	}
	total := 0
	for _, st := range plan.Stages {
		total += len(st.Jobs)
	}
	if total != 1 || plan.Stages[0].Jobs[0].Name != "lint" {
		t.Fatalf("plan should contain only lint, got %d jobs", total)
	}

	// Unknown --job still fails loud at build time.
	if _, err := Build(p, "ghost"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Build(only=ghost) = %v, want not-found", err)
	}

	// Matrix base + cell targeting still work through the filter.
	pm, err := parser.Parse(strings.NewReader(`
name: m
stages: [t]
jobs:
  unit:
    stage: t
    image: alpine:3.20
    script: ["true"]
    parallel:
      matrix:
        - GO: ["1.24", "1.25"]
`), "proj", "m")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	planAll, err := Build(pm, "unit")
	if err != nil || len(planAll.Stages[0].Jobs) != 2 {
		t.Fatalf("base-name target should run both cells: %v", err)
	}
	planOne, err := Build(pm, "unit [GO=1.25]")
	if err != nil || len(planOne.Stages[0].Jobs) != 1 {
		t.Fatalf("cell target should run one cell: %v", err)
	}
}
