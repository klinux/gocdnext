package runner

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

func TestAssignmentPreferredNodeAffinity(t *testing.T) {
	a := &gocdnextv1.JobAssignment{
		PreferredNodeAffinity: []*gocdnextv1.PreferredNodeAffinityTerm{{
			Weight: 100,
			MatchExpressions: []*gocdnextv1.NodeAffinityMatchExpression{
				{Key: "cloud.google.com/gke-spot", Operator: "In", Values: []string{"true"}},
			},
		}},
	}
	out := assignmentPreferredNodeAffinity(a)
	if len(out) != 1 || out[0].Weight != 100 {
		t.Fatalf("unexpected result: %+v", out)
	}
	me := out[0].Preference.MatchExpressions
	if len(me) != 1 || me[0].Key != "cloud.google.com/gke-spot" ||
		me[0].Operator != corev1.NodeSelectorOpIn || len(me[0].Values) != 1 || me[0].Values[0] != "true" {
		t.Errorf("unexpected expression: %+v", me)
	}
}

func TestAssignmentPreferredNodeAffinity_EmptyIsNil(t *testing.T) {
	if got := assignmentPreferredNodeAffinity(&gocdnextv1.JobAssignment{}); got != nil {
		t.Errorf("expected nil for empty input, got %+v", got)
	}
}
