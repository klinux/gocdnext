package scheduler

import (
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// preferredNodeAffinityToProto is the resolve → JobAssignment conversion. It
// must map every field and deep-copy values so a shipped assignment is
// independent of the resolved profile.
func TestPreferredNodeAffinityToProto(t *testing.T) {
	in := []store.PreferredNodeAffinityTerm{{
		Weight: 100,
		MatchExpressions: []store.NodeAffinityMatchExpression{
			{Key: "cloud.google.com/gke-spot", Operator: "In", Values: []string{"true"}},
		},
	}}
	out := preferredNodeAffinityToProto(in)
	if len(out) != 1 || out[0].GetWeight() != 100 {
		t.Fatalf("weight/len: %+v", out)
	}
	me := out[0].GetMatchExpressions()
	if len(me) != 1 || me[0].GetKey() != "cloud.google.com/gke-spot" ||
		me[0].GetOperator() != "In" || len(me[0].GetValues()) != 1 || me[0].GetValues()[0] != "true" {
		t.Errorf("expression lost: %+v", me)
	}

	if preferredNodeAffinityToProto(nil) != nil {
		t.Error("empty input should map to nil")
	}

	// Deep-copy: mutating the input's values must not affect the wire output.
	in[0].MatchExpressions[0].Values[0] = "MUTATED"
	if out[0].GetMatchExpressions()[0].GetValues()[0] != "true" {
		t.Error("proto output aliased the input slice")
	}
}
