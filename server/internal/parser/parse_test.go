package parser

import (
	"fmt"
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

func TestParse_JobOutputs_AcceptsDeclaration(t *testing.T) {
	// outputs: maps a YAML alias (lowercase, kebab-ish) to the
	// plugin env-var name read from $GOCDNEXT_OUTPUT_FILE. Both
	// charsets validated; the parsed shape lands on
	// domain.Job.Outputs verbatim.
	const y = `
name: release
stages: [bump]
materials:
  - manual: true
jobs:
  bump:
    stage: bump
    uses: ghcr.io/klinux/gocdnext-plugin-semver-bump@v1
    outputs:
      next: NEXT
      kind: KIND
      image-digest: PROMOTED_DIGEST
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	bump := findJob(t, p, "bump")
	if got, want := len(bump.Outputs), 3; got != want {
		t.Fatalf("outputs len = %d, want %d (%v)", got, want, bump.Outputs)
	}
	cases := map[string]string{
		"next":         "NEXT",
		"kind":         "KIND",
		"image-digest": "PROMOTED_DIGEST",
	}
	for alias, want := range cases {
		if got := bump.Outputs[alias]; got != want {
			t.Errorf("outputs[%q] = %q, want %q", alias, got, want)
		}
	}
}

// TestParse_JobOutputs_ObjectFormCarriesMaskedFlag — issue #22.
// The schema accepts both short and object forms; the object form
// passes the `masked: true` opt-in through to domain.Job.OutputMasks
// so the scheduler can add the resolved value to LogMasks at
// dispatch.
func TestParse_JobOutputs_ObjectFormCarriesMaskedFlag(t *testing.T) {
	const y = `
name: release
stages: [bump]
materials:
  - manual: true
jobs:
  bump:
    stage: bump
    uses: ghcr.io/klinux/gocdnext-plugin-semver-bump@v1
    outputs:
      next: NEXT                       # short form — not masked
      release-token:
        env: RELEASE_TOKEN              # object form — masked opt-in
        masked: true
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	bump := findJob(t, p, "bump")
	if got := bump.Outputs["next"]; got != "NEXT" {
		t.Errorf("outputs[next] = %q, want NEXT", got)
	}
	if got := bump.Outputs["release-token"]; got != "RELEASE_TOKEN" {
		t.Errorf("outputs[release-token] = %q, want RELEASE_TOKEN", got)
	}
	if bump.OutputMasks["release-token"] != true {
		t.Errorf("output_masks[release-token] = %v, want true (object form opt-in)", bump.OutputMasks["release-token"])
	}
	if bump.OutputMasks["next"] {
		t.Errorf("output_masks[next] = true, want false (short form keeps default)")
	}
}

// TestParse_JobOutputs_ObjectFormRejectsUnknownKeys — issue #22.
// `masked` is a security-adjacent flag; a typo (`mask:` missing
// `e`, or `env_var:` instead of `env:`) MUST fail loud at parse
// rather than silently landing as `masked=false`.
//
// The outer parser uses KnownFields(true), but that strictness
// doesn't propagate into a Node.Decode inside an UnmarshalYAML;
// OutputDef.UnmarshalYAML walks the mapping keys manually.
func TestParse_JobOutputs_ObjectFormRejectsUnknownKeys(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // substring expected in the error
	}{
		{
			name: "mask typo (missing e)",
			yaml: `
name: release
stages: [bump]
materials: [{manual: true}]
jobs:
  bump:
    stage: bump
    uses: ghcr.io/klinux/gocdnext-plugin-semver-bump@v1
    outputs:
      release-token:
        env: RELEASE_TOKEN
        mask: true
`,
			want: `unknown key "mask"`,
		},
		{
			name: "env_var instead of env",
			yaml: `
name: release
stages: [bump]
materials: [{manual: true}]
jobs:
  bump:
    stage: bump
    uses: ghcr.io/klinux/gocdnext-plugin-semver-bump@v1
    outputs:
      release-token:
        env_var: RELEASE_TOKEN
`,
			want: `unknown key "env_var"`,
		},
		{
			name: "completely made-up key",
			yaml: `
name: release
stages: [bump]
materials: [{manual: true}]
jobs:
  bump:
    stage: bump
    uses: ghcr.io/klinux/gocdnext-plugin-semver-bump@v1
    outputs:
      release-token:
        env: RELEASE_TOKEN
        secret: true
`,
			want: `unknown key "secret"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseNamed(strings.NewReader(tc.yaml), "p", "release")
			if err == nil {
				t.Fatalf("parse: expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse: error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParse_JobOutputs_RejectsBadShape(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "alias starts with uppercase",
			yaml: outputsYAML("Next", "NEXT"),
		},
		{
			name: "alias starts with digit",
			yaml: outputsYAML("1next", "NEXT"),
		},
		{
			name: "alias contains slash",
			yaml: outputsYAML("next/x", "NEXT"),
		},
		{
			name: "env-name starts with digit",
			yaml: outputsYAML("next", "1NEXT"),
		},
		{
			name: "env-name contains dash",
			yaml: outputsYAML("next", "MY-VAR"),
		},
		{
			name: "env-name empty",
			yaml: outputsYAML("next", ""),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseNamed(strings.NewReader(tc.yaml), "p", "release")
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

// outputsYAML builds a one-job pipeline whose outputs block carries
// the given alias/env pair. The rest of the YAML is the smallest
// shape Parse will accept so the validation failure is the only
// reason a test case fails.
func outputsYAML(alias, envName string) string {
	return `
name: release
stages: [bump]
materials:
  - manual: true
jobs:
  bump:
    stage: bump
    uses: ghcr.io/klinux/gocdnext-plugin-semver-bump@v1
    outputs:
      ` + alias + `: ` + envName + `
`
}

// findJob walks the parsed jobs slice for a name match — the
// in-memory shape is ordered, not keyed, so this is the test-only
// O(n) lookup helper.
func findJob(t *testing.T, p *domain.Pipeline, name string) domain.Job {
	t.Helper()
	for _, j := range p.Jobs {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("job %q not in parsed pipeline (jobs=%v)", name, p.Jobs)
	return domain.Job{}
}

func TestParse_TriggerEvents_AcceptsTag(t *testing.T) {
	// `event: [tag]` is a first-class trigger value alongside push +
	// pull_request — pipelines auto-fire on any tag push for the
	// project's repo, routed by URL only (the webhook handler
	// matches URL + filters by Events). This test guards against
	// future validation that whitelists only push+pull_request
	// and would silently drop tag.
	const y = `
name: release
when:
  event: [tag]
stages: [build]
materials:
  - manual: true
jobs:
  build:
    stage: build
    image: alpine
    script: [echo hi]
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.TriggerEvents) != 1 || p.TriggerEvents[0] != "tag" {
		t.Errorf("TriggerEvents = %v, want [tag]", p.TriggerEvents)
	}
}

func TestParse_GitMaterialEvents_AcceptsTag(t *testing.T) {
	// Explicit git material with `on: [push, tag]` — both events
	// land on the Material.Events list. The webhook handler uses
	// this list to filter which materials fire on which event.
	const y = `
name: release
stages: [build]
materials:
  - git:
      url: https://github.com/org/repo
      branch: main
      on: [push, tag]
jobs:
  build:
    stage: build
    image: alpine
    script: [echo hi]
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Materials) != 1 || p.Materials[0].Git == nil {
		t.Fatalf("materials = %+v, want one git material", p.Materials)
	}
	got := p.Materials[0].Git.Events
	if len(got) != 2 || got[0] != "push" || got[1] != "tag" {
		t.Errorf("Events = %v, want [push tag]", got)
	}
}

func TestParse_TriggerEvents_RejectsTypo(t *testing.T) {
	// `event: [tags]` (note the trailing s) used to apply cleanly
	// then silently never fire — the field is enum-validated since
	// v0.10.0 so the parse fails loud at apply time. Same for
	// arbitrary garbage values.
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "tags plural typo",
			yaml: `
name: release
when:
  event: [tags]
stages: [build]
materials:
  - manual: true
jobs:
  build:
    stage: build
    image: alpine
    script: [echo hi]
`,
		},
		{
			name: "unknown garbage value",
			yaml: `
name: release
when:
  event: [push, weekend]
stages: [build]
materials:
  - manual: true
jobs:
  build:
    stage: build
    image: alpine
    script: [echo hi]
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseNamed(strings.NewReader(tc.yaml), "p", "release")
			if err == nil {
				t.Fatal("expected error on invalid event, got nil")
			}
		})
	}
}

func TestParse_GitMaterialEvents_RejectsTypo(t *testing.T) {
	// Git material `on:` field — same enum guard, smaller accepted
	// set (push, pull_request, tag — cron/manual/upstream are
	// pipeline-level concepts, not per-material).
	const y = `
name: release
stages: [build]
materials:
  - git:
      url: https://github.com/org/repo
      branch: main
      on: [push, tagg]
jobs:
  build:
    stage: build
    image: alpine
    script: [echo hi]
`
	_, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err == nil {
		t.Fatal("expected error on invalid material event, got nil")
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

func TestParse_AgentProfile(t *testing.T) {
	// `agent.profile` binds the job to a runner profile by name.
	// `agent.tags` are extra constraints that union with the
	// top-level `tags:` so neither origin can silently veto the
	// other at scheduling.
	y := `
stages: [build]
materials:
  - manual: true
jobs:
  build:
    stage: build
    script: [make build]
    tags: [linux]
    agent:
      profile: gpu
      tags: [accelerator-v100]
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	j := p.Jobs[0]
	if j.Profile != "gpu" {
		t.Fatalf("profile = %q, want gpu", j.Profile)
	}
	want := []string{"linux", "accelerator-v100"}
	if len(j.Tags) != len(want) || j.Tags[0] != want[0] || j.Tags[1] != want[1] {
		t.Fatalf("tags = %+v, want %v", j.Tags, want)
	}
}

func TestParse_Resources(t *testing.T) {
	y := `
stages: [build]
materials:
  - manual: true
jobs:
  build:
    stage: build
    script: [make]
    resources:
      requests: { cpu: "500m", memory: "512Mi" }
      limits:   { cpu: "2",    memory: "2Gi" }
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := p.Jobs[0].Resources
	if r.Requests.CPU != "500m" || r.Requests.Memory != "512Mi" {
		t.Fatalf("requests = %+v", r.Requests)
	}
	if r.Limits.CPU != "2" || r.Limits.Memory != "2Gi" {
		t.Fatalf("limits = %+v", r.Limits)
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

// TestParse_ArtifactsDedupeByCanonicalForm — the storage layer's
// partial unique index on (job_run_id, path) normalizes trailing
// slashes (`dist/` collapses to `dist`). Without parser-side
// canonical dedupe, a job like
//
//	paths:    [dist, dist/]
//	optional: [dist/, screenshots]
//
// produces an ArtifactPaths list with `dist` AND `dist/` — the
// agent uploader's own dedupe collapses them, fine for required.
// But required + optional run as TWO separate Upload calls in the
// runner; the second batch's server-side insert for `dist/`
// canonicalizes to `dist`, hits the unique index because the
// required `dist` row is already there, transaction rolls back,
// and `screenshots` is lost as collateral. Parser-level canonical
// dedupe makes the proto wire shape carry a clean separation.
func TestParse_ArtifactsDedupeByCanonicalForm(t *testing.T) {
	y := `
stages: [build]
materials:
  - manual: true
jobs:
  build:
    stage: build
    script: [make]
    artifacts:
      paths: [dist, dist/]
      optional: [dist/, screenshots]
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	j := p.Jobs[0]

	// Within paths: `dist` and `dist/` collapse — first-occurrence
	// shape (`dist`) wins.
	if len(j.ArtifactPaths) != 1 || j.ArtifactPaths[0] != "dist" {
		t.Fatalf("required = %v, want [dist]", j.ArtifactPaths)
	}
	// Across lists: optional `dist/` is dropped because required
	// `dist` already covers it (canonical match). `screenshots`
	// survives.
	if len(j.OptionalArtifactPaths) != 1 || j.OptionalArtifactPaths[0] != "screenshots" {
		t.Fatalf("optional = %v, want [screenshots] (dist/ canonical-deduped against required)", j.OptionalArtifactPaths)
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

func TestParse_JobCache(t *testing.T) {
	// Jobs opt into named caches via a list of {key, paths}
	// entries. The agent will fetch each cache (silent miss on
	// first run) before tasks and re-upload after success, so a
	// pnpm store / go build cache survives across runs without
	// paying artifact transfer on every job.
	y := `
stages: [test]
materials:
  - manual: true
jobs:
  deps:
    stage: test
    image: golang:1.25
    script: [go build ./...]
    cache:
      - key: go-build
        paths: [~/.cache/go-build]
      - key: pnpm-store
        paths: [web/.pnpm-store, web/node_modules/.cache]
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	j := p.Jobs[0]
	if len(j.Cache) != 2 {
		t.Fatalf("cache entries = %d, want 2", len(j.Cache))
	}
	if j.Cache[0].Key != "go-build" || j.Cache[0].Paths[0] != "~/.cache/go-build" {
		t.Errorf("cache[0] = %+v", j.Cache[0])
	}
	if j.Cache[1].Key != "pnpm-store" || len(j.Cache[1].Paths) != 2 {
		t.Errorf("cache[1] = %+v", j.Cache[1])
	}
}

func TestParse_JobCacheRequiresKeyAndPaths(t *testing.T) {
	// Missing key or empty paths is a config bug — the agent
	// would have nothing to store or nothing to fetch. Fail
	// loud at parse time.
	cases := map[string]string{
		"missing key": `
stages: [test]
materials: [{manual: true}]
jobs:
  x:
    stage: test
    image: alpine
    script: [echo]
    cache:
      - paths: [some/path]
`,
		"empty paths": `
stages: [test]
materials: [{manual: true}]
jobs:
  x:
    stage: test
    image: alpine
    script: [echo]
    cache:
      - key: noop
        paths: []
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

func TestParse_ApprovalGate(t *testing.T) {
	// Minimal approval job: no script, no image, just the
	// `approval:` sub-object with approvers + description. Parser
	// must translate it into domain.Job.Approval and leave Tasks
	// empty so the scheduler's dispatch path never tries to run
	// it as a script job.
	y := `
stages: [deploy]
materials: [{manual: true}]
jobs:
  release-prod:
    stage: deploy
    approval:
      approvers: [alice, bob]
      description: "Ready to ship prod?"
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Jobs) != 1 {
		t.Fatalf("jobs = %d", len(p.Jobs))
	}
	j := p.Jobs[0]
	if j.Approval == nil {
		t.Fatal("Approval nil on approval job")
	}
	if got := j.Approval.Approvers; len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("approvers = %+v", got)
	}
	if j.Approval.Description != "Ready to ship prod?" {
		t.Errorf("description = %q", j.Approval.Description)
	}
	if len(j.Tasks) != 0 {
		t.Errorf("Tasks = %+v; approval gates must have no tasks", j.Tasks)
	}
}

func TestParse_ApprovalRejectsMixedWithExecutionKnobs(t *testing.T) {
	// An approval gate that also declares script/image/etc is a
	// config bug: the scheduler never dispatches an approval job,
	// so any execution field the user wrote would silently never
	// run. Fail loudly so the bug surfaces at apply time, not six
	// pushes later when someone notices the script didn't run.
	cases := map[string]string{
		"approval + script": `
stages: [deploy]
materials: [{manual: true}]
jobs:
  x:
    stage: deploy
    approval: {approvers: [a]}
    script: [echo not-this]
`,
		"approval + image": `
stages: [deploy]
materials: [{manual: true}]
jobs:
  x:
    stage: deploy
    approval: {approvers: [a]}
    image: alpine
`,
		"approval + uses": `
stages: [deploy]
materials: [{manual: true}]
jobs:
  x:
    stage: deploy
    approval: {approvers: [a]}
    uses: gocdnext/node
`,
	}
	for label, y := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := Parse(strings.NewReader(y), "p", "n")
			if err == nil {
				t.Fatal("expected parse error")
			}
		})
	}
}

func TestParse_ApprovalQuorumByLabel(t *testing.T) {
	// quorum_by_label gives the operator a per-PR-label override
	// of the base `required` quorum. Maps lowercased label name →
	// new quorum value. Resolved at run materialisation time:
	// PR labels (from cause_detail.pr_labels) are intersected with
	// the map keys; if any match, the LARGEST override wins
	// (defensive — two reasons to demand more shouldn't cancel).
	y := `
stages: [deploy]
materials: [{manual: true}]
jobs:
  release-prod:
    stage: deploy
    approval:
      approver_groups: [sre, platform, security]
      required: 2
      quorum_by_label:
        hotfix: 1
        risky:  3
      description: "Promote to prod?"
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := p.Jobs[0].Approval
	if a == nil {
		t.Fatal("Approval nil")
	}
	if a.Required != 2 {
		t.Errorf("required = %d, want 2", a.Required)
	}
	if got := a.QuorumByLabel["hotfix"]; got != 1 {
		t.Errorf("hotfix override = %d, want 1", got)
	}
	if got := a.QuorumByLabel["risky"]; got != 3 {
		t.Errorf("risky override = %d, want 3", got)
	}
	if len(a.QuorumByLabel) != 2 {
		t.Errorf("QuorumByLabel size = %d, want 2", len(a.QuorumByLabel))
	}
}

func TestParse_ApprovalQuorumByLabelValidation(t *testing.T) {
	// Each invalid input must surface at parse time so the
	// operator catches it before runtime. We test the failure
	// modes the resolver would otherwise have to defend against.
	cases := map[string]string{
		"label with shell meta": `
stages: [deploy]
materials: [{manual: true}]
jobs:
  g:
    stage: deploy
    approval:
      approvers: [a, b]
      required: 2
      quorum_by_label:
        "bad$label": 1
`,
		"override below 1": `
stages: [deploy]
materials: [{manual: true}]
jobs:
  g:
    stage: deploy
    approval:
      approvers: [a, b]
      required: 2
      quorum_by_label:
        hotfix: 0
`,
		"override exceeds approver pool": `
stages: [deploy]
materials: [{manual: true}]
jobs:
  g:
    stage: deploy
    approval:
      approvers: [a, b]
      required: 2
      quorum_by_label:
        risky: 5
`,
		"empty label name": `
stages: [deploy]
materials: [{manual: true}]
jobs:
  g:
    stage: deploy
    approval:
      approvers: [a, b]
      required: 2
      quorum_by_label:
        "": 1
`,
	}
	for label, y := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := Parse(strings.NewReader(y), "p", "n")
			if err == nil {
				t.Fatalf("expected parse error for %q", label)
			}
		})
	}
}

func TestParse_ApprovalQuorumByLabelCap(t *testing.T) {
	// Hard cap on entries — 16 distinct labels covers the most
	// elaborate workflow (hotfix, risky, breaking-change, ...).
	// Higher means the operator is encoding policy in YAML the
	// wrong way; tell them so at parse time.
	var labels strings.Builder
	for i := 0; i < 17; i++ {
		fmt.Fprintf(&labels, "        label-%d: 1\n", i)
	}
	y := `
stages: [deploy]
materials: [{manual: true}]
jobs:
  g:
    stage: deploy
    approval:
      approvers: [a, b]
      required: 2
      quorum_by_label:
` + labels.String()
	_, err := Parse(strings.NewReader(y), "p", "n")
	if err == nil {
		t.Fatal("expected parse error for >16 quorum_by_label entries")
	}
}

func TestParse_ApprovalEmptyApproversAllowed(t *testing.T) {
	// Empty approvers = "any authenticated user". Fine for dev
	// and demo pipelines; prod-grade policies layer on top via
	// RBAC later. Don't reject at parse time.
	y := `
stages: [deploy]
materials: [{manual: true}]
jobs:
  gate:
    stage: deploy
    approval:
      description: "Promote?"
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Jobs[0].Approval == nil {
		t.Fatal("Approval nil")
	}
	if got := p.Jobs[0].Approval.Approvers; len(got) != 0 {
		t.Errorf("approvers = %+v, want empty", got)
	}
}

func TestParse_Notifications(t *testing.T) {
	y := `
stages: [build]
materials: [{manual: true}]
jobs:
  b:
    stage: build
    image: golang:1.23
    script: [go build ./...]
notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: https://hooks.slack.example/abc
      channel: "#eng"
    secrets: [SLACK_WEBHOOK]
  - on: success
    uses: gocdnext/email@v1
    with:
      host: smtp.example.com
      from: ci@example.com
      to: team@example.com
      subject: "Success"
      body: "Shipped"
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Notifications) != 2 {
		t.Fatalf("notifications = %d", len(p.Notifications))
	}
	if p.Notifications[0].On != domain.NotifyOnFailure {
		t.Errorf("first trigger = %q", p.Notifications[0].On)
	}
	if p.Notifications[0].Uses != "gocdnext/slack@v1" {
		t.Errorf("first uses = %q", p.Notifications[0].Uses)
	}
	if p.Notifications[0].With["channel"] != "#eng" {
		t.Errorf("first with[channel] missing: %+v", p.Notifications[0].With)
	}
	if got := p.Notifications[0].Secrets; len(got) != 1 || got[0] != "SLACK_WEBHOOK" {
		t.Errorf("first secrets = %+v", got)
	}
	if p.Notifications[1].On != domain.NotifyOnSuccess {
		t.Errorf("second trigger = %q", p.Notifications[1].On)
	}
}

func TestParse_NotificationRejectsUnknownTrigger(t *testing.T) {
	y := `
stages: [build]
materials: [{manual: true}]
jobs:
  b:
    stage: build
    image: x
    script: ["true"]
notifications:
  - on: flaky
    uses: gocdnext/slack@v1
`
	if _, err := Parse(strings.NewReader(y), "p", "n"); err == nil ||
		!strings.Contains(err.Error(), "unknown on") {
		t.Fatalf("want unknown-on error, got %v", err)
	}
}

func TestParse_RejectsReservedStageName(t *testing.T) {
	y := `
stages: [build, _notifications]
materials: [{manual: true}]
jobs:
  b:
    stage: build
    image: x
    script: ["true"]
`
	if _, err := Parse(strings.NewReader(y), "p", "n"); err == nil ||
		!strings.Contains(err.Error(), "reserved") {
		t.Fatalf("want reserved-name error, got %v", err)
	}
}

func TestParse_NotificationRequiresUses(t *testing.T) {
	y := `
stages: [build]
materials: [{manual: true}]
jobs:
  b:
    stage: build
    image: x
    script: ["true"]
notifications:
  - on: always
`
	if _, err := Parse(strings.NewReader(y), "p", "n"); err == nil ||
		!strings.Contains(err.Error(), "uses:") {
		t.Fatalf("want uses-required error, got %v", err)
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
