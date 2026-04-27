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
