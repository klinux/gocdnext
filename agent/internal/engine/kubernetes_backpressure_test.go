package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// enhanceYourCalm is the exact shape the apiserver path surfaces when an
// HTTP/2 RST_STREAM with ENHANCE_YOUR_CALM is received: grpc-go maps it to
// ResourceExhausted. Reproduces the publish-card prep failure.
func enhanceYourCalm() error {
	return status.Error(codes.ResourceExhausted,
		"stream terminated by RST_STREAM with error code: ENHANCE_YOUR_CALM")
}

// reactorFailNThenPass installs a Get-pods reactor that returns err for the
// first n calls, then falls through to the tracker (the seeded pod).
func reactorFailNThenPass(cli *fake.Clientset, n int, err error) {
	var mu sync.Mutex
	calls := 0
	cli.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= n {
			return true, nil, err
		}
		return false, nil, nil // fall through to the tracker
	})
}

// PRIMARY regression: the failure the operator saw was "wait for prep:" —
// which is WaitForInitTerminated, not WaitForInitStarted. Two transient
// ENHANCE_YOUR_CALM Gets must be retried, then the terminated prep init
// container's exit code returned.
func TestWaitForInitTerminated_RetriesTransientBackpressure(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "prep-pod", Namespace: "ci"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "prep",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
				},
			}},
		},
	})
	reactorFailNThenPass(client, 2, enhanceYourCalm())

	k := NewKubernetesWithClient(client, KubernetesConfig{
		Namespace:      "ci",
		PollInterval:   time.Millisecond,
		StartupTimeout: 5 * time.Second,
	})

	exit, err := k.WaitForInitTerminated(context.Background(), "prep-pod", "prep")
	if err != nil {
		t.Fatalf("expected transient backpressure to be retried, got err: %v", err)
	}
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
}

// Additional coverage: WaitForInitStarted ("prep startup timeout:" path).
func TestWaitForInitStarted_RetriesTransientBackpressure(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "prep-pod", Namespace: "ci"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  "prep",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	})
	reactorFailNThenPass(client, 2, enhanceYourCalm())

	k := NewKubernetesWithClient(client, KubernetesConfig{
		Namespace:      "ci",
		PollInterval:   time.Millisecond,
		StartupTimeout: 5 * time.Second,
	})

	if err := k.WaitForInitStarted(context.Background(), "prep-pod", "prep"); err != nil {
		t.Fatalf("expected transient backpressure to be retried, got err: %v", err)
	}
}

// A non-transient error must still abort — retry is scoped to backpressure.
func TestWaitForInitTerminated_FatalErrorStillFails(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, kerrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "prep-pod", nil)
	})
	k := NewKubernetesWithClient(client, KubernetesConfig{
		Namespace:      "ci",
		PollInterval:   time.Millisecond,
		StartupTimeout: 200 * time.Millisecond,
	})

	if _, err := k.WaitForInitTerminated(context.Background(), "prep-pod", "prep"); err == nil {
		t.Fatal("expected Forbidden to remain fatal, got nil")
	}
}
