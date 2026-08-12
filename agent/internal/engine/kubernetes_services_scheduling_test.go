package engine_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// The service pod applies the inherited profile scheduling exactly like a task
// pod: agent baseline MERGED with the per-service hints (profile wins on
// node-selector key collision, tolerations concatenated, preferred affinity set).
func TestEnsureServices_ServicePodInheritsScheduling(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: time.Second,
		NodeSelector:   map[string]string{"agent-baseline": "yes"},
	})

	const podName = "gocdnext-svc-runsched1234-g0-postgres"
	assignPodIPAsync(t, cli, "gocdnext-tests", podName, "10.0.0.10", 5*time.Millisecond)

	_, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{
			Name:         "postgres",
			Image:        "postgres:16",
			NodeSelector: map[string]string{"pool": "gradle"},
			Tolerations: []corev1.Toleration{
				{Key: "ci-only", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule},
			},
			PreferredNodeAffinity: []corev1.PreferredSchedulingTerm{{
				Weight: 50,
				Preference: corev1.NodeSelectorTerm{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: "cloud.google.com/gke-spot", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}},
					},
				},
			}},
		}},
		"runsched1234", "job-1", 0, nil, nil)
	if err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}

	pod, gerr := cli.CoreV1().Pods("gocdnext-tests").Get(context.Background(), podName, metav1.GetOptions{})
	if gerr != nil {
		t.Fatalf("get service pod: %v", gerr)
	}
	// Baseline + inherited selector both present.
	if pod.Spec.NodeSelector["agent-baseline"] != "yes" || pod.Spec.NodeSelector["pool"] != "gradle" {
		t.Errorf("NodeSelector = %v, want agent-baseline=yes + inherited pool=gradle", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "ci-only" {
		t.Errorf("Tolerations = %v, want inherited ci-only", pod.Spec.Tolerations)
	}
	aff := pod.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil ||
		len(aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("expected inherited preferred node affinity, got %+v", aff)
	}
	if w := aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Weight; w != 50 {
		t.Errorf("affinity weight = %d, want 50", w)
	}
}

// Empty per-service scheduling => baseline only, preserving prior behaviour
// (no Affinity block, only the agent baseline node selector).
func TestEnsureServices_ServicePodBaselineOnlyWhenNoInheritedScheduling(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: time.Second,
		NodeSelector:   map[string]string{"agent-baseline": "yes"},
	})

	const podName = "gocdnext-svc-runbaseline0-g0-postgres"
	assignPodIPAsync(t, cli, "gocdnext-tests", podName, "10.0.0.10", 5*time.Millisecond)

	if _, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		"runbaseline0", "job-1", 0, nil, nil); err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}

	pod, gerr := cli.CoreV1().Pods("gocdnext-tests").Get(context.Background(), podName, metav1.GetOptions{})
	if gerr != nil {
		t.Fatalf("get service pod: %v", gerr)
	}
	if pod.Spec.NodeSelector["agent-baseline"] != "yes" {
		t.Errorf("NodeSelector = %v, want agent baseline preserved", pod.Spec.NodeSelector)
	}
	if pod.Spec.Affinity != nil {
		t.Errorf("Affinity = %+v, want nil when no inherited affinity", pod.Spec.Affinity)
	}
}
