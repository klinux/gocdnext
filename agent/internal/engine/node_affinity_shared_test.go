package engine_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

func spotPreferredTerm() corev1.PreferredSchedulingTerm {
	return corev1.PreferredSchedulingTerm{
		Weight: 100,
		Preference: corev1.NodeSelectorTerm{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: "cloud.google.com/gke-spot", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}},
			},
		},
	}
}

func TestBuildPodSpec_SetsPreferredNodeAffinity(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Script:                "true",
		PreferredNodeAffinity: []corev1.PreferredSchedulingTerm{spotPreferredTerm()},
	})
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		t.Fatal("expected node affinity to be set")
	}
	prefs := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(prefs) != 1 || prefs[0].Weight != 100 {
		t.Fatalf("unexpected prefs: %+v", prefs)
	}
	me := prefs[0].Preference.MatchExpressions
	if len(me) != 1 || me[0].Key != "cloud.google.com/gke-spot" || me[0].Operator != corev1.NodeSelectorOpIn {
		t.Errorf("unexpected match expression: %+v", me)
	}
}

func TestBuildPodSpec_NoAffinityWhenEmpty(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	pod := k.BuildPodSpec(engine.ScriptSpec{Script: "true"})
	if pod.Spec.Affinity != nil {
		t.Errorf("expected nil affinity when none configured, got %+v", pod.Spec.Affinity)
	}
}
