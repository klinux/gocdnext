package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func writeSeed(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}

func TestSeed_Affinity_RoundTrip(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	path := writeSeed(t, `
profiles:
  - name: spot-pool
    engine: kubernetes
    preferred_node_affinity:
      - weight: 100
        match_expressions:
          - key: cloud.google.com/gke-spot
            operator: In
            values: ["true"]
`)
	if _, err := s.SeedRunnerProfilesFromFile(ctx, path); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := s.GetRunnerProfileByName(ctx, "spot-pool")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(got.PreferredNodeAffinity) != 1 || got.PreferredNodeAffinity[0].Weight != 100 {
		t.Fatalf("affinity not seeded: %+v", got.PreferredNodeAffinity)
	}
	me := got.PreferredNodeAffinity[0].MatchExpressions
	if len(me) != 1 || me[0].Key != "cloud.google.com/gke-spot" || me[0].Values[0] != "true" {
		t.Errorf("seeded match expression: %+v", me)
	}
}

// KnownFields(true): a typo must be a loud parse error, not a silently-empty
// field + successful boot — both at the top level and inside match_expressions.
func TestSeed_RejectsUnknownField(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	topLevelTypo := writeSeed(t, `
profiles:
  - name: x
    engine: kubernetes
    preferred_node_affinty: []
`)
	if _, err := s.SeedRunnerProfilesFromFile(ctx, topLevelTypo); err == nil {
		t.Error("expected error for unknown top-level field, got nil")
	}

	nestedTypo := writeSeed(t, `
profiles:
  - name: y
    engine: kubernetes
    preferred_node_affinity:
      - weight: 100
        match_expressions:
          - kee: cloud.google.com/gke-spot
            operator: Exists
`)
	if _, err := s.SeedRunnerProfilesFromFile(ctx, nestedTypo); err == nil {
		t.Error("expected error for unknown field inside match_expressions, got nil")
	}
}

// A stray second `---` document must be rejected, not silently ignored —
// matching the admin JSON API's trailing-content rejection.
func TestSeed_RejectsMultipleDocuments(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	path := writeSeed(t, `
profiles:
  - name: first
    engine: kubernetes
---
profiles:
  - name: ignored
    engine: kubernetes
`)
	if _, err := s.SeedRunnerProfilesFromFile(ctx, path); err == nil {
		t.Error("expected error for multiple YAML documents, got nil")
	}
}
