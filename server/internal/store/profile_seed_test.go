package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestSeedRunnerProfilesFromFile_InsertsAndUpdates(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.yaml")
	if err := os.WriteFile(path, []byte(`
profiles:
  - name: default
    description: vanilla pool
    engine: kubernetes
    default_image: alpine:3.20
    max_cpu: "4"
    max_mem: 8Gi
    tags: [linux]
  - name: gpu
    engine: kubernetes
    default_image: nvidia/cuda:12-runtime
    max_cpu: "8"
    max_mem: 32Gi
    tags: [gpu]
`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// First pass: both profiles created.
	if n, err := s.SeedRunnerProfilesFromFile(ctx, path); err != nil || n != 2 {
		t.Fatalf("first pass: n=%d err=%v", n, err)
	}
	got, err := s.GetRunnerProfileByName(ctx, "default")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.MaxCPU != "4" || got.DefaultImage != "alpine:3.20" {
		t.Fatalf("default = %+v", got)
	}

	// Edit + re-seed: existing rows update in place.
	if err := os.WriteFile(path, []byte(`
profiles:
  - name: default
    description: budget enforced
    engine: kubernetes
    default_image: alpine:3.20
    max_cpu: "2"
    max_mem: 4Gi
    tags: [linux]
`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n, err := s.SeedRunnerProfilesFromFile(ctx, path); err != nil || n != 1 {
		t.Fatalf("second pass: n=%d err=%v", n, err)
	}
	got, _ = s.GetRunnerProfileByName(ctx, "default")
	if got.MaxCPU != "2" || got.Description != "budget enforced" {
		t.Fatalf("update did not persist: %+v", got)
	}

	// gpu wasn't in the second YAML — must NOT have been deleted.
	if _, err := s.GetRunnerProfileByName(ctx, "gpu"); err != nil {
		t.Fatalf("gpu was wiped on re-seed: %v", err)
	}
}

func TestSeedRunnerProfilesFromFile_RoundTripsNodeSelectorAndTolerations(t *testing.T) {
	// Regression: the seed loader must persist node_selector +
	// tolerations from the YAML. Without these, the seed loop's
	// UpdateRunnerProfileFromSeed would silently overwrite
	// operator-set scheduling hints with `{}`/`[]` on every server
	// boot (Helm seed vs. UI edit race).
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.yaml")
	if err := os.WriteFile(path, []byte(`
profiles:
  - name: gradle-pool
    engine: kubernetes
    default_image: alpine:3.20
    node_selector:
      workload: ci
      pool: gradle-heavy
    tolerations:
      - key: ci-only
        operator: Equal
        value: "true"
        effect: NoSchedule
      - key: spot
        operator: Exists
        effect: NoExecute
        toleration_seconds: 60
`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// First seed: inserts the profile with scheduling hints.
	if _, err := s.SeedRunnerProfilesFromFile(ctx, path); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	got, err := s.GetRunnerProfileByName(ctx, "gradle-pool")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.NodeSelector["workload"] != "ci" || got.NodeSelector["pool"] != "gradle-heavy" {
		t.Errorf("NodeSelector not seeded: %+v", got.NodeSelector)
	}
	if len(got.Tolerations) != 2 {
		t.Fatalf("Tolerations len = %d", len(got.Tolerations))
	}
	if got.Tolerations[1].TolerationSeconds == nil || *got.Tolerations[1].TolerationSeconds != 60 {
		t.Errorf("TolerationSeconds = %v", got.Tolerations[1].TolerationSeconds)
	}

	// Re-seed (the load-bearing path: this is what happens on every
	// server boot). Hints must NOT be wiped to empty.
	if _, err := s.SeedRunnerProfilesFromFile(ctx, path); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	got2, err := s.GetRunnerProfileByName(ctx, "gradle-pool")
	if err != nil {
		t.Fatalf("post-reseed lookup: %v", err)
	}
	if got2.NodeSelector["pool"] != "gradle-heavy" {
		t.Errorf("NodeSelector wiped on re-seed: %+v", got2.NodeSelector)
	}
	if len(got2.Tolerations) != 2 {
		t.Errorf("Tolerations wiped on re-seed: %+v", got2.Tolerations)
	}
}

func TestSeedRunnerProfilesFromFile_EmptyPathIsNoop(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	if n, err := s.SeedRunnerProfilesFromFile(context.Background(), ""); err != nil || n != 0 {
		t.Fatalf("empty path: n=%d err=%v", n, err)
	}
}

func TestSeedRunnerProfilesFromFile_RejectsBadYAML(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte(`profiles:
  - name: nope
`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.SeedRunnerProfilesFromFile(context.Background(), path); err == nil {
		t.Fatalf("expected engine-required error")
	}
}
