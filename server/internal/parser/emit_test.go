package parser

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// TestEmit_RoundTrip is the load-bearing test: parse a reasonably
// rich YAML → domain.Pipeline → emit → parse again, and check the
// two domain.Pipeline values are structurally equal. If that
// holds, operators editing pipelines from the UI (eventually) or
// reading the "yaml" tab get something that actually matches what
// the server stored — no missing fields, no silent drift.
func TestEmit_RoundTrip(t *testing.T) {
	const src = `
name: demo
stages: [build, test, deploy]
concurrency: serial
variables:
  GO_VERSION: "1.25"
  LOG_LEVEL: debug
materials:
  - git:
      url: https://github.com/org/repo
      branch: main
      on: [push, pull_request]
      auto_register_webhook: true
  - upstream:
      pipeline: build-core
      stage: test
  - manual: true
jobs:
  vet:
    stage: build
    image: golang:1.25
    script:
      - go vet ./...
    tags: [linux, amd64]
  test:
    stage: test
    image: golang:1.25
    needs: [vet]
    docker: true
    script:
      - go test -race ./...
    artifacts:
      paths: [coverage.out]
      optional: [screenshots/]
    secrets:
      - GH_TOKEN
  deploy:
    stage: deploy
    image: registry.local/deployer:1
    needs: [test]
    script:
      - ./deploy.sh
    needs_artifacts:
      - from_job: test
        paths: [coverage.out]
        dest: ./in
`
	first, err := ParseNamed(strings.NewReader(src), "p", "demo")
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}

	out, err := Emit(first)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	// Must be decodable (no unknown fields, no bad indent).
	var sanity File
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	dec.KnownFields(true)
	if err := dec.Decode(&sanity); err != nil {
		t.Fatalf("emitted YAML fails KnownFields decode: %v\n---\n%s", err, out)
	}

	second, err := ParseNamed(strings.NewReader(string(out)), "p", "demo")
	if err != nil {
		t.Fatalf("re-parse emitted YAML: %v\n---\n%s", err, out)
	}

	assertPipelineEqual(t, first, second, out)
}

func TestEmit_StageOrderStable(t *testing.T) {
	// Jobs are unordered in domain (slice built from map iteration in
	// the parser). The emitter has to bucket them by the pipeline's
	// declared stages and sort within each bucket so two emissions of
	// the same pipeline are byte-identical — otherwise the yaml tab
	// flickers every reload.
	p := &domain.Pipeline{
		Name:   "x",
		Stages: []string{"build", "test"},
		Materials: []domain.Material{
			{Type: domain.MaterialManual, Fingerprint: domain.ManualFingerprint()},
		},
		Jobs: []domain.Job{
			{Name: "zeta", Stage: "build", Image: "a", Tasks: []domain.Task{{Script: "a"}}},
			{Name: "alpha", Stage: "build", Image: "a", Tasks: []domain.Task{{Script: "a"}}},
			{Name: "unit", Stage: "test", Image: "a", Tasks: []domain.Task{{Script: "a"}}},
		},
	}
	out1, err := Emit(p)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := Emit(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(out1) != string(out2) {
		t.Fatalf("emit not stable:\n---first---\n%s\n---second---\n%s", out1, out2)
	}
	// alpha should appear before zeta within the build stage.
	a := strings.Index(string(out1), "alpha:")
	z := strings.Index(string(out1), "zeta:")
	if a < 0 || z < 0 || a > z {
		t.Fatalf("expected alphabetical job order inside stage; got:\n%s", out1)
	}
}

func TestEmit_SkipsImplicitMaterials(t *testing.T) {
	// Implicit materials (project repo synthesized at apply time
	// from scm_source) must not appear in the emitted YAML — the
	// "yaml" tab is meant to mirror what the operator wrote, not
	// the stored+synthesized form. They'll be re-synthesized on
	// the next apply either way.
	p := &domain.Pipeline{
		Name:   "ci-server",
		Stages: []string{"build"},
		Materials: []domain.Material{
			{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint("https://github.com/klinux/gocdnext", "main"),
				Implicit:    true,
				Git: &domain.GitMaterial{
					URL: "https://github.com/klinux/gocdnext", Branch: "main",
					Events: []string{"push", "pull_request"}, AutoRegisterWebhook: true,
				},
			},
		},
		Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
	}
	out, err := Emit(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "materials:") {
		t.Errorf("implicit materials should be hidden; emitted:\n%s", out)
	}
	if strings.Contains(string(out), "github.com/klinux/gocdnext") {
		t.Errorf("implicit material url leaked; emitted:\n%s", out)
	}
}

func TestEmit_ExplicitMaterialsStillVisible(t *testing.T) {
	// Upstream / template / sibling-repo materials are operator
	// intent — they must survive round-trip even when an implicit
	// material rides alongside.
	p := &domain.Pipeline{
		Name:   "ci-web",
		Stages: []string{"build"},
		Materials: []domain.Material{
			{
				Type:        domain.MaterialUpstream,
				Fingerprint: domain.UpstreamFingerprint("ci-server", "test"),
				Upstream:    &domain.UpstreamMaterial{Pipeline: "ci-server", Stage: "test"},
			},
			{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint("https://github.com/klinux/gocdnext", "main"),
				Implicit:    true, // synthesized — hide
				Git: &domain.GitMaterial{URL: "https://github.com/klinux/gocdnext", Branch: "main"},
			},
		},
		Jobs: []domain.Job{{Name: "bundle", Stage: "build", Tasks: []domain.Task{{Script: "pnpm build"}}}},
	}
	out, err := Emit(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "upstream:") || !strings.Contains(s, "ci-server") {
		t.Errorf("explicit upstream material missing; emitted:\n%s", s)
	}
	if strings.Contains(s, "github.com/klinux/gocdnext") {
		t.Errorf("implicit material leaked through despite explicit upstream; emitted:\n%s", s)
	}
}

func TestEmit_RoundTripsServices(t *testing.T) {
	// Services (pipeline-level sidecars) must survive parse → emit
	// → re-parse untouched so the yaml tab reflects what the
	// operator wrote. Env map + command slice are the fiddly
	// bits — both need to round-trip structurally, not by pointer
	// identity.
	const src = `
name: ci-integration
stages: [test]
materials:
  - manual: true
services:
  - image: postgres:16-alpine
    env:
      POSTGRES_PASSWORD: test
  - name: cache
    image: redis:7
    command: [redis-server, --appendonly, "no"]
jobs:
  integration:
    stage: test
    image: golang:1.25
    script: [go test ./...]
`
	first, err := ParseNamed(strings.NewReader(src), "p", "ci-integration")
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	out, err := Emit(first)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	second, err := ParseNamed(strings.NewReader(string(out)), "p", "ci-integration")
	if err != nil {
		t.Fatalf("re-parse: %v\n---\n%s", err, out)
	}
	if len(second.Services) != 2 {
		t.Fatalf("services lost in round-trip: %+v\n---\n%s", second.Services, out)
	}
	if second.Services[0].Name != "postgres" ||
		second.Services[0].Image != "postgres:16-alpine" ||
		second.Services[0].Env["POSTGRES_PASSWORD"] != "test" {
		t.Errorf("postgres service mangled: %+v", second.Services[0])
	}
	if second.Services[1].Name != "cache" ||
		len(second.Services[1].Command) != 3 ||
		second.Services[1].Command[0] != "redis-server" {
		t.Errorf("cache service mangled: %+v", second.Services[1])
	}
}

func TestEmit_RoundTripsPluginJobs(t *testing.T) {
	// Plugin jobs emit as `uses:` + `with:` (the ergonomic
	// spelling), not the legacy `image:` + `settings:` form — a
	// parsed legacy job migrates forward on its next emit, which
	// is a deliberate one-way conversion so the yaml tab shows
	// one canonical shape.
	const src = `
name: deploy-flow
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
	first, err := ParseNamed(strings.NewReader(src), "p", "deploy-flow")
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	out, err := Emit(first)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "uses: gocdnext/node") {
		t.Errorf("emit missing uses: — got:\n%s", s)
	}
	if !strings.Contains(s, "with:") {
		t.Errorf("emit missing with: — got:\n%s", s)
	}
	if strings.Contains(s, "settings:") || strings.Contains(s, "image:") {
		t.Errorf("emit leaked legacy spelling — got:\n%s", s)
	}
	// Re-parse round-trip to confirm the emitted form stays
	// structurally equivalent.
	second, err := ParseNamed(strings.NewReader(s), "p", "deploy-flow")
	if err != nil {
		t.Fatalf("re-parse: %v\n---\n%s", err, s)
	}
	plug := second.Jobs[0].Tasks[0].Plugin
	if plug == nil || plug.Image != "gocdnext/node" ||
		plug.Settings["command"] != "build" ||
		plug.Settings["node-version"] != "22" {
		t.Errorf("round-trip lost plugin data: %+v", plug)
	}
}

func TestEmit_LegacyImageSettingsMigratesToUsesWith(t *testing.T) {
	// YAMLs written before uses/with used the legacy `image:` +
	// `settings:` plugin shape. Parsing them still works, but
	// emit writes the new spelling — one canonical form on disk.
	const legacy = `
name: x
stages: [deploy]
materials:
  - manual: true
jobs:
  notify:
    stage: deploy
    image: plugins/slack
    settings:
      channel: "#deploys"
`
	p, err := ParseNamed(strings.NewReader(legacy), "p", "x")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Emit(p)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "uses: plugins/slack") {
		t.Errorf("legacy didn't migrate to uses: — %s", s)
	}
	if strings.Contains(s, "settings:") {
		t.Errorf("legacy settings: leaked to emit — %s", s)
	}
}

func TestEmit_RoundTripsJobCache(t *testing.T) {
	// Cache entries survive parse → emit → re-parse with their
	// keys and paths intact. Structural round-trip is what the
	// yaml tab relies on to show the operator what they wrote.
	const src = `
name: cached
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
        paths: [web/.pnpm-store]
`
	first, err := ParseNamed(strings.NewReader(src), "p", "cached")
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	out, err := Emit(first)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "cache:") || !strings.Contains(s, "key: go-build") {
		t.Errorf("emit missing cache block:\n%s", s)
	}
	second, err := ParseNamed(strings.NewReader(s), "p", "cached")
	if err != nil {
		t.Fatalf("re-parse: %v\n---\n%s", err, s)
	}
	if len(second.Jobs[0].Cache) != 2 {
		t.Fatalf("cache lost in round-trip: %+v", second.Jobs[0].Cache)
	}
}

func TestEmit_QuotesAmbiguousScalars(t *testing.T) {
	// GO_VERSION: "1.25" must round-trip as a quoted string.
	// Without explicit quoting yaml.v3 emits `1.25` → re-parses as
	// a float, breaking the map[string]string round-trip.
	p := &domain.Pipeline{
		Name:   "demo",
		Stages: []string{"build"},
		Materials: []domain.Material{
			{Type: domain.MaterialManual, Fingerprint: domain.ManualFingerprint()},
		},
		Variables: map[string]string{
			"GO_VERSION": "1.25",
			"DEBUG":      "true",
			"ENV":        "prod",
		},
		Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
	}
	out, err := Emit(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `GO_VERSION: "1.25"`) {
		t.Errorf("numeric-looking version should be quoted; emitted:\n%s", s)
	}
	if !strings.Contains(s, `DEBUG: "true"`) {
		t.Errorf("boolean-looking value should be quoted; emitted:\n%s", s)
	}
	// Plain strings shouldn't get over-quoted — keeps the emit
	// readable. `ENV: prod` is unambiguous and stays unquoted.
	if strings.Contains(s, `ENV: "prod"`) {
		t.Errorf("unambiguous string got over-quoted; emitted:\n%s", s)
	}
}

func TestEmit_SmallSequencesUseFlowStyle(t *testing.T) {
	// Short scalar lists (stages, needs, events, paths) should be
	// inline `[a, b, c]` — matches how operators write them by
	// hand and keeps the tab compact.
	p := &domain.Pipeline{
		Name:   "demo",
		Stages: []string{"build", "test"},
		Materials: []domain.Material{
			{Type: domain.MaterialManual, Fingerprint: domain.ManualFingerprint()},
		},
		TriggerEvents: []string{"push", "pull_request"},
		Jobs: []domain.Job{
			{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}},
				ArtifactPaths: []string{"bin/x"}},
			{Name: "unit", Stage: "test", Needs: []string{"compile"},
				Tasks: []domain.Task{{Script: "go test"}}},
		},
	}
	out, err := Emit(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"stages: [build, test]",
		"event: [push, pull_request]",
		"needs: [compile]",
		"paths: [bin/x]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected flow-style %q in output:\n%s", want, s)
		}
	}
}

func TestEmit_MinimalPipeline(t *testing.T) {
	// Minimal: one manual material, one stage, one image-less job.
	// Ensures Emit doesn't crash on the smallest valid pipeline and
	// that round-trip still holds.
	p := &domain.Pipeline{
		Name:   "tiny",
		Stages: []string{"run"},
		Materials: []domain.Material{
			{Type: domain.MaterialManual, Fingerprint: domain.ManualFingerprint()},
		},
		Jobs: []domain.Job{
			{Name: "one", Stage: "run", Tasks: []domain.Task{{Script: "echo hi"}}},
		},
	}
	out, err := Emit(p)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(string(out), "echo hi") {
		t.Fatalf("expected script to be preserved, got:\n%s", out)
	}
	back, err := ParseNamed(strings.NewReader(string(out)), "p", "tiny")
	if err != nil {
		t.Fatalf("re-parse: %v\n---\n%s", err, out)
	}
	if back.Name != "tiny" || len(back.Jobs) != 1 || back.Jobs[0].Name != "one" {
		t.Fatalf("round-trip lost data: %+v", back)
	}
}

// assertPipelineEqual ignores ordering of unordered fields and
// cross-checks the handful of bits we actually care about: name,
// stages, materials by fingerprint, jobs by name, and each job's
// image + script + needs + tags + secrets + artifact lists + docker
// flag. The domain uses slices for what YAML expresses as maps so
// iteration order shifts trip plain reflect.DeepEqual.
func assertPipelineEqual(t *testing.T, a, b *domain.Pipeline, emitted []byte) {
	t.Helper()
	if a.Name != b.Name {
		t.Errorf("name: %q vs %q", a.Name, b.Name)
	}
	if strings.Join(a.Stages, ",") != strings.Join(b.Stages, ",") {
		t.Errorf("stages: %v vs %v", a.Stages, b.Stages)
	}
	if a.Concurrency != b.Concurrency {
		t.Errorf("concurrency: %q vs %q", a.Concurrency, b.Concurrency)
	}
	if len(a.Materials) != len(b.Materials) {
		t.Errorf("materials len: %d vs %d\n---\n%s", len(a.Materials), len(b.Materials), emitted)
	}
	fpA := map[string]bool{}
	for _, m := range a.Materials {
		fpA[m.Fingerprint] = true
	}
	for _, m := range b.Materials {
		if !fpA[m.Fingerprint] {
			t.Errorf("material %s missing after round-trip\n---\n%s", m.Fingerprint, emitted)
		}
	}
	jobsA := map[string]domain.Job{}
	for _, j := range a.Jobs {
		jobsA[j.Name] = j
	}
	if len(a.Jobs) != len(b.Jobs) {
		t.Errorf("jobs len: %d vs %d", len(a.Jobs), len(b.Jobs))
	}
	for _, jb := range b.Jobs {
		ja, ok := jobsA[jb.Name]
		if !ok {
			t.Errorf("job %q missing after round-trip", jb.Name)
			continue
		}
		if ja.Stage != jb.Stage {
			t.Errorf("job %q stage: %q vs %q", jb.Name, ja.Stage, jb.Stage)
		}
		if ja.Image != jb.Image {
			t.Errorf("job %q image: %q vs %q", jb.Name, ja.Image, jb.Image)
		}
		if ja.Docker != jb.Docker {
			t.Errorf("job %q docker: %v vs %v", jb.Name, ja.Docker, jb.Docker)
		}
		if !sliceEq(ja.Needs, jb.Needs) {
			t.Errorf("job %q needs: %v vs %v", jb.Name, ja.Needs, jb.Needs)
		}
		if !sliceEq(ja.Tags, jb.Tags) {
			t.Errorf("job %q tags: %v vs %v", jb.Name, ja.Tags, jb.Tags)
		}
		if !sliceEq(ja.Secrets, jb.Secrets) {
			t.Errorf("job %q secrets: %v vs %v", jb.Name, ja.Secrets, jb.Secrets)
		}
		if !sliceEq(ja.ArtifactPaths, jb.ArtifactPaths) {
			t.Errorf("job %q artifactPaths: %v vs %v", jb.Name, ja.ArtifactPaths, jb.ArtifactPaths)
		}
		if !sliceEq(ja.OptionalArtifactPaths, jb.OptionalArtifactPaths) {
			t.Errorf("job %q optionalArtifactPaths: %v vs %v", jb.Name, ja.OptionalArtifactPaths, jb.OptionalArtifactPaths)
		}
		if len(ja.Tasks) != len(jb.Tasks) {
			t.Errorf("job %q tasks len: %d vs %d", jb.Name, len(ja.Tasks), len(jb.Tasks))
			continue
		}
		for i := range ja.Tasks {
			if ja.Tasks[i].Script != jb.Tasks[i].Script {
				t.Errorf("job %q task[%d].Script: %q vs %q", jb.Name, i, ja.Tasks[i].Script, jb.Tasks[i].Script)
			}
		}
		if len(ja.ArtifactDeps) != len(jb.ArtifactDeps) {
			t.Errorf("job %q needs_artifacts len: %d vs %d", jb.Name, len(ja.ArtifactDeps), len(jb.ArtifactDeps))
			continue
		}
		for i := range ja.ArtifactDeps {
			if ja.ArtifactDeps[i].FromJob != jb.ArtifactDeps[i].FromJob ||
				ja.ArtifactDeps[i].Dest != jb.ArtifactDeps[i].Dest ||
				!sliceEq(ja.ArtifactDeps[i].Paths, jb.ArtifactDeps[i].Paths) {
				t.Errorf("job %q needs_artifacts[%d] differ: %+v vs %+v",
					jb.Name, i, ja.ArtifactDeps[i], jb.ArtifactDeps[i])
			}
		}
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
