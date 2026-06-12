package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFolder_Fanout(t *testing.T) {
	// Uses the examples/fanout fixture from the repo — 3 pipelines in one folder.
	got, err := LoadFolder("../../../examples/fanout", "", "fanout-proj")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 pipelines, got %d", len(got))
	}

	want := []string{"build-core", "deploy-api", "deploy-worker"}
	for i, p := range got {
		if p.Name != want[i] {
			t.Errorf("[%d] name: want %q, got %q", i, want[i], p.Name)
		}
	}

	// The two downstreams must reference the upstream pipeline.
	for _, p := range got[1:] {
		if len(p.Materials) != 1 || p.Materials[0].Upstream == nil {
			t.Errorf("%s: expected single upstream material", p.Name)
			continue
		}
		if p.Materials[0].Upstream.Pipeline != "build-core" {
			t.Errorf("%s: upstream.pipeline: want build-core, got %s",
				p.Name, p.Materials[0].Upstream.Pipeline)
		}
	}
}

func TestLoadFolder_FilenameFallback(t *testing.T) {
	// examples/matrix has one file (cross-build.yaml) with an explicit name
	// field matching the filename — serves as a sanity check.
	got, err := LoadFolder("../../../examples/matrix", "", "matrix-proj")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].Name != "cross-build" {
		t.Fatalf("unexpected: %+v", got)
	}
}

// TestLoadFolder_SingleFileMode covers the GitLab-CI-style path
// where config_path itself points at a single YAML — e.g.
// ".gocdnext.yml" at the repo root — instead of a folder. Parser
// should take the file's filename (without extension) as the
// fallback pipeline name.
func TestLoadFolder_SingleFileMode(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, ".gocdnext.yml")
	yaml := `
name: ci
stages: [build]
materials:
  - git:
      url: https://github.com/org/demo
      branch: main
      on: [push]
jobs:
  compile:
    stage: build
    script: [make]
`
	if err := os.WriteFile(file, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := LoadFolder(root, ".gocdnext.yml", "single-proj")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 pipeline, got %d", len(got))
	}
	if got[0].Name != "ci" {
		t.Fatalf("name = %q, want ci", got[0].Name)
	}
}

func TestIsSingleFileConfigPath(t *testing.T) {
	cases := map[string]bool{
		"":                false,
		".gocdnext":       false,
		".gocdnext/":      false,
		".gocdnext.yml":   true,
		".gocdnext.yaml":  true,
		"apps/api.yaml":   true,
		"apps/api/folder": false,
		"pipeline.ymlx":   false,
	}
	for in, want := range cases {
		if got := IsSingleFileConfigPath(in); got != want {
			t.Errorf("IsSingleFileConfigPath(%q) = %v, want %v", in, got, want)
		}
	}
}
