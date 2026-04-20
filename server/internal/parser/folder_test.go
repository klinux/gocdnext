package parser

import (
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
