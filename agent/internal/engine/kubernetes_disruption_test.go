package engine

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func disruptionEngine(client *fake.Clientset) *Kubernetes {
	return NewKubernetesWithClient(client, KubernetesConfig{Namespace: "ci"})
}

// A kubelet eviction (resource pressure) → Disrupted, with the k8s reason
// + message surfaced so the operator sees WHY, not a bare exit 143.
func TestTaskPodTermination_EvictedIsDisrupted(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ci"},
		Status: corev1.PodStatus{
			Phase:   corev1.PodFailed,
			Reason:  "Evicted",
			Message: "The node was low on resource: memory.",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "task",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 143}},
			}},
		},
	})
	term := disruptionEngine(client).TaskPodTermination(context.Background(), "p")
	if term.PodGone {
		t.Error("PodGone = true, want false (pod still exists)")
	}
	if !term.Disrupted {
		t.Error("Disrupted = false, want true (Evicted)")
	}
	if term.Reason != "Evicted" {
		t.Errorf("Reason = %q, want Evicted", term.Reason)
	}
	if term.Message == "" {
		t.Error("Message empty, want the eviction detail")
	}
}

// Pod object gone (deleted/preempted/node reclaimed) → PodGone+Disrupted,
// so the runner skips the housekeeper scans (no "container not found").
func TestTaskPodTermination_PodGone(t *testing.T) {
	term := disruptionEngine(fake.NewSimpleClientset()).TaskPodTermination(context.Background(), "missing")
	if !term.PodGone || !term.Disrupted {
		t.Errorf("PodGone=%v Disrupted=%v, want both true", term.PodGone, term.Disrupted)
	}
}

// A process inside the job took SIGTERM on its own — pod healthy, no
// disruption reason. Must NOT be flagged disrupted, so the runner keeps
// scanning test reports (they may still carry signal).
func TestTaskPodTermination_HealthyExit143NotDisrupted(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ci"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "task", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 143, Reason: "Error"}}},
				{Name: "housekeeper", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	})
	term := disruptionEngine(client).TaskPodTermination(context.Background(), "p")
	if term.PodGone || term.Disrupted {
		t.Errorf("PodGone=%v Disrupted=%v, want both false (self-SIGTERM, pod healthy)", term.PodGone, term.Disrupted)
	}
}

// kubelet losing the container (node went away) shows up as
// ContainerStatusUnknown — treat as disrupted even without a pod-level
// reason.
func TestTaskPodTermination_ContainerStatusUnknownIsDisrupted(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ci"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "task",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown"}},
			}},
		},
	})
	term := disruptionEngine(client).TaskPodTermination(context.Background(), "p")
	if !term.Disrupted {
		t.Error("Disrupted = false, want true (ContainerStatusUnknown = node lost the container)")
	}
}
