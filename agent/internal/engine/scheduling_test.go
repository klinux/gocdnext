package engine

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestMergeNodeSelector_NilOnBothEmpty(t *testing.T) {
	if got := mergeNodeSelector(nil, nil); got != nil {
		t.Errorf("nil+nil = %v, want nil", got)
	}
	if got := mergeNodeSelector(map[string]string{}, nil); got != nil {
		t.Errorf("empty+nil = %v, want nil", got)
	}
}

func TestMergeNodeSelector_AgentOnly(t *testing.T) {
	got := mergeNodeSelector(map[string]string{"tier": "ci"}, nil)
	if got["tier"] != "ci" {
		t.Errorf("agent-only = %v", got)
	}
}

func TestMergeNodeSelector_ProfileWinsOnCollision(t *testing.T) {
	// Profile is more specific than agent default; a job declaring
	// `profile: gradle-heavy` with `pool: gradle` should land on a
	// gradle node even if the agent's StatefulSet says `pool: ci`.
	agent := map[string]string{"tier": "ci", "pool": "ci"}
	profile := map[string]string{"pool": "gradle"}
	got := mergeNodeSelector(agent, profile)
	if got["tier"] != "ci" {
		t.Errorf("agent key not preserved: %v", got)
	}
	if got["pool"] != "gradle" {
		t.Errorf("profile did not win on collision: pool = %v", got["pool"])
	}
}

func TestMergeNodeSelector_DoesNotMutateInputs(t *testing.T) {
	// A pod-spec builder mutating its inputs could rewrite the
	// agent's NodeSelector for every future job on the same agent.
	// Both inputs must survive the call untouched.
	agent := map[string]string{"tier": "ci"}
	profile := map[string]string{"pool": "gradle"}
	_ = mergeNodeSelector(agent, profile)
	if len(agent) != 1 || agent["tier"] != "ci" {
		t.Errorf("agent mutated: %v", agent)
	}
	if len(profile) != 1 || profile["pool"] != "gradle" {
		t.Errorf("profile mutated: %v", profile)
	}
}

func TestConcatTolerations_NilOnBothEmpty(t *testing.T) {
	if got := concatTolerations(nil, nil); got != nil {
		t.Errorf("nil+nil = %v, want nil", got)
	}
}

func TestConcatTolerations_PreservesOrderAndIndependence(t *testing.T) {
	agent := []corev1.Toleration{
		{Key: "node.kubernetes.io/unschedulable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}
	profile := []corev1.Toleration{
		{Key: "ci-only", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule},
		{Key: "spot", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	}
	got := concatTolerations(agent, profile)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Key != "node.kubernetes.io/unschedulable" {
		t.Errorf("agent must come first: %v", got)
	}
	if got[1].Key != "ci-only" || got[2].Key != "spot" {
		t.Errorf("profile order lost: %v", got)
	}

	// Mutating the returned slice must not back-propagate into the
	// inputs (defensive copy guards against a future caller cache
	// reusing the agent slice across pods).
	got[0].Key = "MUTATED"
	if agent[0].Key != "node.kubernetes.io/unschedulable" {
		t.Errorf("mutation leaked into agent: %v", agent)
	}
}

func TestConcatTolerations_DeepCopiesTolerationSeconds(t *testing.T) {
	// Regression: a naive `append(out, in...)` would copy the struct
	// but alias the *TolerationSeconds pointer. cloneToleration must
	// allocate a fresh *int64 so a later mutation on either side
	// can't leak across.
	seconds := int64(60)
	agent := []corev1.Toleration{
		{Key: "spot", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
	}
	got := concatTolerations(agent, nil)
	if got[0].TolerationSeconds == nil || *got[0].TolerationSeconds != 60 {
		t.Fatalf("seconds round-trip failed: %v", got[0].TolerationSeconds)
	}

	// Mutate the cloned int64; agent slice's pointer must stay at 60.
	*got[0].TolerationSeconds = 999
	if *agent[0].TolerationSeconds != 60 {
		t.Errorf("TolerationSeconds aliased agent input; mutation leaked: %d",
			*agent[0].TolerationSeconds)
	}

	// Same direction in reverse: mutating the original local var
	// (which agent[0] points to) must not change the cloned copy.
	seconds = 1234
	if *got[0].TolerationSeconds != 999 {
		t.Errorf("clone aliased the input var; got[0] now reads %d",
			*got[0].TolerationSeconds)
	}
}

func TestCloneToleration_NilTolerationSecondsStaysNil(t *testing.T) {
	t1 := corev1.Toleration{Key: "k", Operator: corev1.TolerationOpEqual}
	got := cloneToleration(t1)
	if got.TolerationSeconds != nil {
		t.Errorf("nil should stay nil, got %v", got.TolerationSeconds)
	}
}
