package store_test

import (
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

func spotAffinity(weight int32) []store.PreferredNodeAffinityTerm {
	return []store.PreferredNodeAffinityTerm{{
		Weight: weight,
		MatchExpressions: []store.NodeAffinityMatchExpression{
			{Key: "cloud.google.com/gke-spot", Operator: "In", Values: []string{"true"}},
		},
	}}
}

// Crosses migration 00083 + sqlc + the raw UPDATE: insert → get → update →
// get → clear → get. The JSONB column, the RETURNING scan, and both raw
// UPDATE statements must all carry preferred_node_affinity.
func TestRunnerProfile_Affinity_RoundTripInsertGetUpdateClear(t *testing.T) {
	s, ctx := newProfileStore(t)

	created, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{
		Name:                  "spot-pool",
		Engine:                "kubernetes",
		PreferredNodeAffinity: spotAffinity(100),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.GetRunnerProfile(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.PreferredNodeAffinity) != 1 || got.PreferredNodeAffinity[0].Weight != 100 {
		t.Fatalf("affinity round-trip: %+v", got.PreferredNodeAffinity)
	}
	me := got.PreferredNodeAffinity[0].MatchExpressions
	if len(me) != 1 || me[0].Key != "cloud.google.com/gke-spot" ||
		me[0].Operator != "In" || len(me[0].Values) != 1 || me[0].Values[0] != "true" {
		t.Errorf("match expression lost: %+v", me)
	}

	// Update to a different affinity (raw UPDATE path).
	if err := s.UpdateRunnerProfile(ctx, nil, created.ID, store.RunnerProfileInput{
		Name:                  "spot-pool",
		Engine:                "kubernetes",
		PreferredNodeAffinity: spotAffinity(50),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = s.GetRunnerProfile(ctx, created.ID)
	if len(got.PreferredNodeAffinity) != 1 || got.PreferredNodeAffinity[0].Weight != 50 {
		t.Errorf("affinity after update: %+v", got.PreferredNodeAffinity)
	}

	// Clear (empty input) → decoder normalises empty to nil.
	if err := s.UpdateRunnerProfile(ctx, nil, created.ID, store.RunnerProfileInput{
		Name:   "spot-pool",
		Engine: "kubernetes",
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = s.GetRunnerProfile(ctx, created.ID)
	if got.PreferredNodeAffinity != nil {
		t.Errorf("affinity not cleared: %+v", got.PreferredNodeAffinity)
	}
}

// ResolveProfileByName (the dispatch path) must surface affinity alongside
// node_selector + tolerations.
func TestRunnerProfile_ResolveByName_CarriesAffinity(t *testing.T) {
	s, ctx := newProfileStore(t)

	if _, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{
		Name:                  "spot",
		Engine:                "kubernetes",
		PreferredNodeAffinity: spotAffinity(90),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	resolved, err := s.ResolveProfileByName(ctx, nil, "spot")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resolved.PreferredNodeAffinity) != 1 || resolved.PreferredNodeAffinity[0].Weight != 90 {
		t.Errorf("resolve did not carry affinity: %+v", resolved.PreferredNodeAffinity)
	}
}
