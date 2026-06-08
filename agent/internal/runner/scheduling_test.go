package runner

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

func TestAssignmentNodeSelector_EmptyReturnsNil(t *testing.T) {
	if got := assignmentNodeSelector(&gocdnextv1.JobAssignment{}); got != nil {
		t.Errorf("empty assignment NodeSelector = %v, want nil", got)
	}
}

func TestAssignmentNodeSelector_CopiesMap(t *testing.T) {
	// Mutating the returned map must not back-propagate into the
	// proto-owned memory; the runner should never mutate proto
	// values, but a defensive copy makes the contract loud.
	a := &gocdnextv1.JobAssignment{
		NodeSelector: map[string]string{"pool": "gradle"},
	}
	got := assignmentNodeSelector(a)
	got["pool"] = "MUTATED"
	if a.NodeSelector["pool"] != "gradle" {
		t.Errorf("mutation leaked into proto: %v", a.NodeSelector)
	}
}

func TestAssignmentTolerations_EmptyReturnsNil(t *testing.T) {
	if got := assignmentTolerations(&gocdnextv1.JobAssignment{}); got != nil {
		t.Errorf("empty assignment Tolerations = %v, want nil", got)
	}
}

func TestAssignmentTolerations_RoundTripsAllFields(t *testing.T) {
	seconds := int64(60)
	a := &gocdnextv1.JobAssignment{
		Tolerations: []*gocdnextv1.Toleration{
			{Key: "ci-only", Operator: "Equal", Value: "true", Effect: "NoSchedule"},
			{Key: "spot", Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &seconds},
		},
	}
	got := assignmentTolerations(a)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Key != "ci-only" ||
		got[0].Operator != corev1.TolerationOpEqual ||
		got[0].Value != "true" ||
		got[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].TolerationSeconds == nil || *got[1].TolerationSeconds != 60 {
		t.Errorf("got[1].TolerationSeconds = %v, want 60", got[1].TolerationSeconds)
	}

	// Aliasing guard mirroring scheduler.tolerationsToProto: mutating
	// the input proto's TolerationSeconds pointer after the helper
	// returned must not change the engine-side struct.
	seconds = 999
	if *got[1].TolerationSeconds != 60 {
		t.Errorf("TolerationSeconds aliased the proto pointer; mutation leaked: %d", *got[1].TolerationSeconds)
	}
}
