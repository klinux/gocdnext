package validate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodYAML = `
name: ci
stages: [test]
jobs:
  unit:
    stage: test
    image: golang:1.26
    script: ["go test ./..."]
`

const badYAML = `
name: broken
stages: [test]
jobs:
  unit:
    stage: nope
    image: golang:1.26
    script: ["true"]
`

func writeFixtures(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".gocdnext")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(cfg, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRun_AllValid(t *testing.T) {
	dir := writeFixtures(t, map[string]string{"ci.yaml": goodYAML})
	var out bytes.Buffer
	if err := Run(&out, dir); err != nil {
		t.Fatalf("Run = %v, want nil; out=%s", err, out.String())
	}
	if !strings.Contains(out.String(), "ci.yaml") || !strings.Contains(out.String(), "OK") {
		t.Fatalf("output missing OK line: %s", out.String())
	}
}

func TestRun_InvalidFileFailsWithFileAndReason(t *testing.T) {
	dir := writeFixtures(t, map[string]string{
		"ci.yaml":     goodYAML,
		"broken.yaml": badYAML,
	})
	var out bytes.Buffer
	err := Run(&out, dir)
	if err == nil {
		t.Fatalf("Run = nil, want error; out=%s", out.String())
	}
	// The valid file still reports OK — one broken file must not
	// hide the state of the others.
	if !strings.Contains(out.String(), "ci.yaml") {
		t.Fatalf("valid file missing from output: %s", out.String())
	}
	if !strings.Contains(out.String(), "broken.yaml") {
		t.Fatalf("broken file missing from output: %s", out.String())
	}
}

func TestRun_DirectGocdnextDir(t *testing.T) {
	dir := writeFixtures(t, map[string]string{"ci.yaml": goodYAML})
	var out bytes.Buffer
	if err := Run(&out, filepath.Join(dir, ".gocdnext")); err != nil {
		t.Fatalf("Run on .gocdnext dir = %v", err)
	}
}

func TestRun_SingleFile(t *testing.T) {
	dir := writeFixtures(t, map[string]string{"ci.yaml": goodYAML})
	var out bytes.Buffer
	if err := Run(&out, filepath.Join(dir, ".gocdnext", "ci.yaml")); err != nil {
		t.Fatalf("Run on single file = %v", err)
	}
}

func TestRun_NoPipelines(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := Run(&out, dir); err == nil {
		t.Fatal("Run on empty dir = nil, want error")
	}
}

// Review-round LOW: set-level duplicate detection (the apply path
// rejects two files declaring the same pipeline name) and the
// extension-less fallback name, both mirrored from apply.go.
func TestRun_DuplicatePipelineNamesFail(t *testing.T) {
	dir := writeFixtures(t, map[string]string{
		"a.yaml": "name: ci\nstages: [t]\njobs:\n  j: {stage: t, image: alpine, script: [\"true\"]}\n",
		"b.yaml": "name: ci\nstages: [t]\njobs:\n  j: {stage: t, image: alpine, script: [\"true\"]}\n",
	})
	var out bytes.Buffer
	err := Run(&out, dir)
	if err == nil {
		t.Fatalf("Run = nil, want duplicate-name failure\n%s", out.String())
	}
	if !strings.Contains(out.String(), "already defined") {
		t.Fatalf("output missing duplicate explanation: %s", out.String())
	}
}

func TestRun_FallbackNameStripsExtension(t *testing.T) {
	dir := writeFixtures(t, map[string]string{
		"nameless.yaml": "stages: [t]\njobs:\n  j: {stage: t, image: alpine, script: [\"true\"]}\n",
	})
	var out bytes.Buffer
	if err := Run(&out, dir); err != nil {
		t.Fatalf("Run = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), `pipeline "nameless"`) {
		t.Fatalf("fallback name should strip .yaml: %s", out.String())
	}
}
