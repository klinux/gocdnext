package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// commitThenBackpressure returns a reactor that COMMITS the object to the fake
// tracker (simulating a server-side commit) but reports ENHANCE_YOUR_CALM to the
// caller — the exact "committed, but the RST_STREAM ate the response" shape that
// makes a create ambiguous.
func commitThenBackpressure(cli *fake.Clientset, resource string) k8stesting.ReactionFunc {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: resource}
	return func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca := a.(k8stesting.CreateAction)
		_ = cli.Tracker().Create(gvr, ca.GetObject(), ca.GetNamespace()) // ignore AlreadyExists on re-attempts
		return true, nil, enhanceYourCalm()
	}
}

// The reviewer's High finding: an ambiguous assignment-Secret create (committed
// server-side, response lost, budget then exhausted) must be reconciled — the
// Secret has no ownerRef yet, so a leak here is never GC'd. After the failure no
// orphan Secret may remain.
func TestCreateIsolatedJobPod_AmbiguousSecretCreateIsCleanedUp(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "secrets", commitThenBackpressure(client, "secrets"))

	k := NewKubernetesWithClient(client, KubernetesConfig{
		Namespace:      "ci",
		WorkspaceMode:  WorkspaceModeIsolated,
		AgentImage:     "agent:v1",
		DefaultImage:   "alpine:3.19",
		WorkspaceSize:  "10Gi",
		PollInterval:   time.Millisecond,
		StartupTimeout: 30 * time.Millisecond, // exhausts mid-backoff → ambiguous
	})

	_, _, err := k.CreateIsolatedJobPod(context.Background(), IsolatedJobSpec{
		RunID:   "r",
		JobID:   "j",
		Image:   "alpine",
		Script:  "echo",
		WorkDir: "/workspace",
	}, []byte("assignment-bytes"))
	if err == nil {
		t.Fatal("expected the ambiguous secret create to fail")
	}
	if !strings.Contains(err.Error(), "create assignment secret") {
		t.Errorf("error should name the secret-create phase: %v", err)
	}

	list, lerr := client.CoreV1().Secrets("ci").List(context.Background(), metav1.ListOptions{})
	if lerr != nil {
		t.Fatalf("list secrets: %v", lerr)
	}
	if len(list.Items) != 0 {
		t.Errorf("ambiguous create left %d orphan secret(s) — leak the reconcile must prevent", len(list.Items))
	}
}

// The review's follow-up: the leak must also be closed on the EXTERNAL-CANCEL
// path. The parent ctx is cancelled right after the first committed transient,
// so the create aborts with context.Canceled (ambiguous). The reconcile must use
// a FRESH ctx — otherwise the delete returns context.Canceled before the wire and
// the ownerRef-less Secret leaks.
func TestCreateIsolatedJobPod_AmbiguousSecretCleanedUpOnExternalCancel(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	var once sync.Once
	client.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca := a.(k8stesting.CreateAction)
		_ = client.Tracker().Create(gvr, ca.GetObject(), ca.GetNamespace())
		once.Do(cancel) // cancel the parent AFTER committing the first attempt
		return true, nil, enhanceYourCalm()
	})

	k := NewKubernetesWithClient(client, KubernetesConfig{
		Namespace:      "ci",
		WorkspaceMode:  WorkspaceModeIsolated,
		AgentImage:     "agent:v1",
		DefaultImage:   "alpine:3.19",
		WorkspaceSize:  "10Gi",
		PollInterval:   time.Millisecond,
		StartupTimeout: 5 * time.Second, // long: the CANCEL drives the exit, not a timeout
	})

	_, _, err := k.CreateIsolatedJobPod(ctx, IsolatedJobSpec{
		RunID:   "r",
		JobID:   "j",
		Image:   "alpine",
		Script:  "echo",
		WorkDir: "/workspace",
	}, []byte("assignment-bytes"))
	if err == nil {
		t.Fatal("expected the cancelled ambiguous create to fail")
	}

	// Fresh-ctx reconcile: the Secret is gone despite the cancelled parent ctx.
	list, lerr := client.CoreV1().Secrets("ci").List(context.Background(), metav1.ListOptions{})
	if lerr != nil {
		t.Fatalf("list secrets: %v", lerr)
	}
	if len(list.Items) != 0 {
		t.Errorf("external-cancel ambiguous create left %d orphan secret(s) — the fresh-ctx reconcile must prevent this", len(list.Items))
	}
}

// The owner-patch failure branch must ALSO clean the Secret under a dead ctx:
// the patch fails (fake pod has empty UID), and the parent ctx is cancelled as
// the pod is created, so the eager secret delete runs under a cancelled ctx. A
// ctx-bound delete would no-op and leak the sensitive assignment payload (this
// branch returns secretName="" → the runner won't retry the cleanup).
func TestCreateIsolatedJobPod_SecretCleanedUpOnOwnerPatchFailureUnderCancel(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		once.Do(cancel)        // cancel the parent as the pod commits
		return false, nil, nil // fall through: the tracker creates the (empty-UID) pod
	})

	k := NewKubernetesWithClient(client, KubernetesConfig{
		Namespace:      "ci",
		WorkspaceMode:  WorkspaceModeIsolated,
		AgentImage:     "agent:v1",
		DefaultImage:   "alpine:3.19",
		WorkspaceSize:  "10Gi",
		PollInterval:   time.Millisecond,
		StartupTimeout: 5 * time.Second,
	})

	_, secretName, err := k.CreateIsolatedJobPod(ctx, IsolatedJobSpec{
		RunID:   "r",
		JobID:   "j",
		Image:   "alpine",
		Script:  "echo",
		WorkDir: "/workspace",
	}, []byte("sensitive-assignment-payload"))
	if err == nil {
		t.Fatal("expected owner-patch failure (fake pod has empty UID)")
	}
	if secretName != "" {
		t.Errorf("secretName should be empty on patch failure, got %q", secretName)
	}

	list, lerr := client.CoreV1().Secrets("ci").List(context.Background(), metav1.ListOptions{})
	if lerr != nil {
		t.Fatalf("list secrets: %v", lerr)
	}
	if len(list.Items) != 0 {
		t.Errorf("owner-patch failure under cancelled ctx left %d orphan secret(s) — sensitive-payload leak", len(list.Items))
	}
}

// Finding 3: assertOurServicePod also validates the service-generation label, so
// a name-collision pod carrying the wrong generation can't be silently adopted.
func TestAssertOurServicePod_ValidatesGeneration(t *testing.T) {
	ourLabels := func(gen string) map[string]string {
		return map[string]string{
			"app.kubernetes.io/managed-by":   "gocdnext-agent",
			"app.kubernetes.io/component":    "service",
			"gocdnext.io/service":            "postgres",
			"gocdnext.io/run-id":             "run1",
			"gocdnext.io/service-generation": gen,
		}
	}
	matching := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: ourLabels("0")}}
	if err := assertOurServicePod(matching, "run1", "postgres", 0); err != nil {
		t.Fatalf("generation 0 should match label 0: %v", err)
	}

	stale := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: ourLabels("0")}}
	if err := assertOurServicePod(stale, "run1", "postgres", 1); err == nil {
		t.Error("generation 1 vs label 0 must be rejected")
	}
}
