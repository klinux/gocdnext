package engine

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestBuildIsolatedJobPodSpec_SetsPreferredNodeAffinity(t *testing.T) {
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "run-A",
		JobID:                "job-B",
		Image:                "node:20",
		Script:               "true",
		WorkDir:              "/workspace",
		AssignmentSecretName: "gocdnext-job-x-assignment",
		PreferredNodeAffinity: []corev1.PreferredSchedulingTerm{{
			Weight: 80,
			Preference: corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "cloud.google.com/gke-spot", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		t.Fatal("expected node affinity to be set")
	}
	prefs := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(prefs) != 1 || prefs[0].Weight != 80 {
		t.Fatalf("unexpected prefs: %+v", prefs)
	}
}
