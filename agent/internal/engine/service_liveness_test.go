package engine

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// livenessPod builds a service pod with the given phase + container
// state, and (optionally) a podIP already assigned — the combination
// that matters here, since Kubernetes keeps status.podIP populated on a
// terminal pod until the object is deleted.
func livenessPod(phase corev1.PodPhase, podIP string, state corev1.ContainerState) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gocdnext-svc-abc-g0-postgres", Namespace: "ci"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "service", Image: "postgres:17-alpine"}},
		},
		Status: corev1.PodStatus{
			Phase:             phase,
			PodIP:             podIP,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "service", State: state}},
		},
	}
}

func running() corev1.ContainerState {
	return corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
}

func terminated(exit int32, reason string) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: exit,
		Reason:   reason,
	}}
}

func TestServicePodDeath(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		// wantDead is whether the pod must be treated as not serving;
		// wantIn are substrings the reason has to carry so an operator
		// can act on the message without reaching for kubectl.
		wantDead bool
		wantIn   []string
	}{
		{
			name:     "nil pod",
			pod:      nil,
			wantDead: false,
		},
		{
			name:     "running with an IP is alive",
			pod:      livenessPod(corev1.PodRunning, "10.1.2.3", running()),
			wantDead: false,
		},
		{
			name: "pending with no container status yet is alive (still starting)",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			},
			wantDead: false,
		},
		{
			name:     "waiting on ContainerCreating is alive (still starting)",
			pod:      livenessPod(corev1.PodPending, "", corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}),
			wantDead: false,
		},
		{
			name: "waiting on ImagePullBackOff is alive for THIS predicate",
			// Startup failure is the wait loop's job to time out, with
			// its own error. Calling it dead here would turn a slow
			// image pull into an immediate hard failure.
			pod:      livenessPod(corev1.PodPending, "", corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}),
			wantDead: false,
		},
		{
			// THE case this predicate exists for: the pod died AFTER it
			// had served, and kubelet still carries its podIP.
			name:     "failed with a stale podIP",
			pod:      livenessPod(corev1.PodFailed, "10.1.2.3", terminated(137, "OOMKilled")),
			wantDead: true,
			wantIn:   []string{"Failed", "exit 137", "OOMKilled"},
		},
		{
			name:     "succeeded with a stale podIP",
			pod:      livenessPod(corev1.PodSucceeded, "10.1.2.3", terminated(0, "Completed")),
			wantDead: true,
			wantIn:   []string{"Succeeded", "exit 0", "Completed"},
		},
		{
			name:     "failed before an IP was ever assigned",
			pod:      livenessPod(corev1.PodFailed, "", terminated(1, "Error")),
			wantDead: true,
			wantIn:   []string{"Failed", "exit 1"},
		},
		{
			// Phase lags container status: kubelet terminated the
			// container but has not written the terminal phase yet.
			// Adopting here is the same bug one poll earlier.
			name:     "container terminated while the phase still says Running",
			pod:      livenessPod(corev1.PodRunning, "10.1.2.3", terminated(137, "OOMKilled")),
			wantDead: true,
			wantIn:   []string{"exit 137", "OOMKilled"},
		},
		{
			name:     "terminated with no reason still reports the exit code",
			pod:      livenessPod(corev1.PodRunning, "10.1.2.3", terminated(2, "")),
			wantDead: true,
			wantIn:   []string{"exit 2"},
		},
		{
			name: "pod-level reason is reported when there is no container status",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", PodIP: "10.1.2.3"},
			},
			wantDead: true,
			wantIn:   []string{"Failed", "Evicted"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := servicePodDeath(tt.pod)
			if tt.wantDead && got == "" {
				t.Fatal("reported alive, want dead")
			}
			if !tt.wantDead && got != "" {
				t.Fatalf("reported dead (%q), want alive", got)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("reason %q missing %q", got, want)
				}
			}
		})
	}
}

// TestServicePodDeath_RestartedButRunningIsAlive separates this
// predicate from the teardown-time one. A container that restarted and
// is running again IS serving: refusing to adopt it would fail a job
// over a service that currently works. "It restarted at some point" is
// a warning (see the teardown snapshot), not a reason to stop.
func TestServicePodDeath_RestartedButRunningIsAlive(t *testing.T) {
	pod := livenessPod(corev1.PodRunning, "10.1.2.3", running())
	pod.Status.ContainerStatuses[0].RestartCount = 3
	pod.Status.ContainerStatuses[0].LastTerminationState = terminated(137, "OOMKilled")

	if got := servicePodDeath(pod); got != "" {
		t.Errorf("a restarted-but-running service must be adoptable; got %q", got)
	}
}

// TestServicePodDeath_IgnoresNonServiceContainers: service pods carry
// exactly one container today, but a future sidecar must not be able to
// condemn the service by exiting on its own.
func TestServicePodDeath_IgnoresNonServiceContainers(t *testing.T) {
	pod := livenessPod(corev1.PodRunning, "10.1.2.3", running())
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name:  "some-future-sidecar",
		State: terminated(0, "Completed"),
	})

	if got := servicePodDeath(pod); got != "" {
		t.Errorf("a terminated non-service container must not condemn the pod; got %q", got)
	}
}
