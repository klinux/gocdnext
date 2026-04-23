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
	p, err := ParseNamed(strings.NewReader(sampleYAML), "proj-1", "my-pipeline")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if p.Name != "my-pipeline" {
		t.Errorf("name: want my-pipeline, got %q", p.Name)
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

func TestParse_NameFromFile(t *testing.T) {
	// YAML has explicit name: — should override the fallback filename.
	const y = `
name: real-name
stages: [build]
materials:
  - manual: true
jobs:
  one:
    stage: build
    image: alpine
    script: [echo hi]
`
	p, err := ParseNamed(strings.NewReader(y), "p", "filename-fallback")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "real-name" {
		t.Errorf("name should be real-name, got %q", p.Name)
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

func TestParse_Secrets(t *testing.T) {
	y := `
stages: [deploy]
materials:
  - manual: true
jobs:
  push:
    stage: deploy
    image: registry:local
    script: [docker push]
    secrets:
      - GH_TOKEN
      - REGISTRY_PASSWORD
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Jobs) != 1 {
		t.Fatalf("jobs = %d", len(p.Jobs))
	}
	got := p.Jobs[0].Secrets
	if len(got) != 2 || got[0] != "GH_TOKEN" || got[1] != "REGISTRY_PASSWORD" {
		t.Fatalf("secrets = %+v", got)
	}
}

func TestParse_Tags(t *testing.T) {
	y := `
stages: [build]
materials:
  - manual: true
jobs:
  build-amd64:
    stage: build
    script: [go build]
    tags: [linux, amd64]
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := p.Jobs[0].Tags
	if len(got) != 2 || got[0] != "linux" || got[1] != "amd64" {
		t.Fatalf("tags = %+v", got)
	}
}

func TestParse_DockerFlagParsed(t *testing.T) {
	// `docker: true` on a job opts into a docker-capable runtime —
	// the parser just lifts the bool into domain.Job.Docker; the
	// scheduler wires it into JobAssignment and each engine
	// decides how to satisfy (socket mount / sidecar). Default is
	// false so legacy jobs stay unchanged.
	y := `
stages: [test]
materials:
  - manual: true
jobs:
  integration:
    stage: test
    image: golang:1.25
    docker: true
    script: [go test -tags=integration ./...]
  unit:
    stage: test
    image: golang:1.25
    script: [go test ./...]
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var integration, unit *domain.Job
	for i := range p.Jobs {
		switch p.Jobs[i].Name {
		case "integration":
			integration = &p.Jobs[i]
		case "unit":
			unit = &p.Jobs[i]
		}
	}
	if integration == nil || unit == nil {
		t.Fatal("expected both jobs parsed")
	}
	if !integration.Docker {
		t.Fatalf("integration should have docker=true")
	}
	if unit.Docker {
		t.Fatalf("unit should have docker=false by default")
	}
}

func TestParse_OptionalArtifactsSplitFromRequired(t *testing.T) {
	// `artifacts.optional:` is kept separate from `paths:` — the
	// parser dedups (required wins if a path appears in both).
	// Runtime semantics: required paths fail the job on upload
	// error, optional paths are best-effort.
	y := `
stages: [build]
materials:
  - manual: true
jobs:
  build:
    stage: build
    script: [make]
    artifacts:
      paths: [bin/agent, bin/server]
      optional: [coverage.xml, bin/agent]
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	j := p.Jobs[0]
	if len(j.ArtifactPaths) != 2 {
		t.Fatalf("required = %v, want 2", j.ArtifactPaths)
	}
	if len(j.OptionalArtifactPaths) != 1 || j.OptionalArtifactPaths[0] != "coverage.xml" {
		t.Fatalf("optional = %v, want [coverage.xml] (bin/agent deduped)", j.OptionalArtifactPaths)
	}
}

func TestParse_NeedsArtifacts(t *testing.T) {
	y := `
stages: [build, deploy]
materials:
  - manual: true
jobs:
  build:
    stage: build
    script: [make]
    artifacts:
      paths: [bin/]
  deploy:
    stage: deploy
    script: [./deploy.sh]
    needs_artifacts:
      - from_job: build
        paths: [bin/]
        dest: ./in
      - from_job: build
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var dep *domain.Job
	for i := range p.Jobs {
		if p.Jobs[i].Name == "deploy" {
			dep = &p.Jobs[i]
		}
	}
	if dep == nil {
		t.Fatal("deploy job not parsed")
	}
	if len(dep.ArtifactDeps) != 2 {
		t.Fatalf("want 2 deps, got %d", len(dep.ArtifactDeps))
	}
	if dep.ArtifactDeps[0].FromJob != "build" ||
		dep.ArtifactDeps[0].Dest != "./in" ||
		len(dep.ArtifactDeps[0].Paths) != 1 {
		t.Errorf("first dep = %+v", dep.ArtifactDeps[0])
	}
	if dep.ArtifactDeps[1].FromJob != "build" ||
		dep.ArtifactDeps[1].Dest != "" ||
		len(dep.ArtifactDeps[1].Paths) != 0 {
		t.Errorf("second dep (defaults) = %+v", dep.ArtifactDeps[1])
	}
}

func TestParse_NeedsArtifacts_MissingFromJob(t *testing.T) {
	y := `
stages: [build]
materials:
  - manual: true
jobs:
  oops:
    stage: build
    script: [true]
    needs_artifacts:
      - paths: [bin/]
`
	if _, err := Parse(strings.NewReader(y), "p", "n"); err == nil {
		t.Fatal("expected error for missing from_job")
	}
}

func TestParse_Services(t *testing.T) {
	// Services are pipeline-level sidecars the agent brings up
	// alongside every job. `name` defaults to the image's short
	// form so `image: postgres:16-alpine` implies `name: postgres`
	// without extra YAML. Env + command pass through verbatim.
	y := `
stages: [test]
materials:
  - manual: true
services:
  - image: postgres:16-alpine
    env:
      POSTGRES_PASSWORD: test
  - name: cache
    image: redis:7
    command: ["redis-server", "--appendonly", "no"]
jobs:
  integration:
    stage: test
    image: golang:1.25
    script: [go test ./...]
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(p.Services))
	}
	pg := p.Services[0]
	if pg.Name != "postgres" {
		t.Errorf("service[0] name = %q, want postgres (derived from image)", pg.Name)
	}
	if pg.Image != "postgres:16-alpine" {
		t.Errorf("service[0] image = %q", pg.Image)
	}
	if pg.Env["POSTGRES_PASSWORD"] != "test" {
		t.Errorf("service[0] env: %+v", pg.Env)
	}
	redis := p.Services[1]
	if redis.Name != "cache" {
		t.Errorf("service[1] name = %q, want cache (explicit override)", redis.Name)
	}
	if len(redis.Command) != 3 || redis.Command[0] != "redis-server" {
		t.Errorf("service[1] command = %+v", redis.Command)
	}
}

func TestParse_ServiceRequiresImage(t *testing.T) {
	bad := `
stages: [test]
materials: [{manual: true}]
services:
  - name: broken
jobs:
  t:
    stage: test
    script: [echo hi]
`
	_, err := Parse(strings.NewReader(bad), "p", "n")
	if err == nil {
		t.Fatal("expected error for service without image")
	}
	if !strings.Contains(err.Error(), "image is required") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestParse_ServiceNameDerivedFromRegistryImage(t *testing.T) {
	// Registry-qualified images ("registry.local/foo/bar:v1")
	// should still yield a dns-label name — strip registry, repo
	// path AND tag.
	y := `
stages: [test]
materials: [{manual: true}]
services:
  - image: registry.local/platform/minio:2025-01
jobs:
  t:
    stage: test
    script: [echo hi]
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Services[0].Name != "minio" {
		t.Fatalf("name = %q, want minio", p.Services[0].Name)
	}
}

func TestParse_PluginJobViaUsesWith(t *testing.T) {
	// `uses:` + `with:` is the ergonomic sugar for a plugin job —
	// parser produces a single PluginStep task carrying image +
	// settings. No `script:` on the job: the plugin image's
	// ENTRYPOINT IS the logic.
	y := `
stages: [deploy]
materials:
  - manual: true
jobs:
  publish:
    stage: deploy
    uses: gocdnext/node
    with:
      command: build
      node-version: "22"
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Jobs) != 1 {
		t.Fatalf("jobs = %d", len(p.Jobs))
	}
	job := p.Jobs[0]
	if len(job.Tasks) != 1 || job.Tasks[0].Plugin == nil {
		t.Fatalf("task shape = %+v", job.Tasks)
	}
	plug := job.Tasks[0].Plugin
	if plug.Image != "gocdnext/node" {
		t.Errorf("plugin image = %q", plug.Image)
	}
	if plug.Settings["command"] != "build" || plug.Settings["node-version"] != "22" {
		t.Errorf("settings round-trip lost: %+v", plug.Settings)
	}
}

func TestParse_PluginRejectsMixedSpellings(t *testing.T) {
	// A job that sets both `uses:` and `image:` is ambiguous — the
	// parser refuses instead of picking one silently. Same for
	// `uses:` alongside `script:`: the plugin's entrypoint runs
	// the logic, a trailing user script would be ignored.
	cases := map[string]string{
		"uses + image": `
stages: [deploy]
materials: [{manual: true}]
jobs:
  x:
    stage: deploy
    uses: plugins/slack
    image: alpine
`,
		"uses + script": `
stages: [deploy]
materials: [{manual: true}]
jobs:
  x:
    stage: deploy
    uses: plugins/slack
    script: [echo hi]
`,
	}
	for label, y := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := Parse(strings.NewReader(y), "p", "n")
			if err == nil {
				t.Fatalf("expected parse error, got nil")
			}
		})
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
