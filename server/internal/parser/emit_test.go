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
