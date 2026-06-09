package runner

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// resultCapture observes JobResult + LogLine messages that the
// runner sends back via the Config.Send callback.
type resultCapture struct {
	mu      sync.Mutex
	results []*gocdnextv1.JobResult
	logs    []string
}

func (rc *resultCapture) send(msg *gocdnextv1.AgentMessage) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	switch k := msg.Kind.(type) {
	case *gocdnextv1.AgentMessage_Result:
		rc.results = append(rc.results, k.Result)
	case *gocdnextv1.AgentMessage_Log:
		rc.logs = append(rc.logs, k.Log.GetText())
	}
}

func TestExecute_Isolated_RejectsMultiTaskJob(t *testing.T) {
	k := engine.NewKubernetesWithClient(fake.NewSimpleClientset(), engine.KubernetesConfig{
		Namespace:     "ci",
		WorkspaceMode: engine.WorkspaceModeIsolated,
		AgentImage:    "agent:v1",
	})

	rc := &resultCapture{}
	r := New(Config{
		Send:   rc.send,
		Engine: k,
	})

	a := &gocdnextv1.JobAssignment{
		RunId: "r", JobId: "j", Name: "multi",
		Tasks: []*gocdnextv1.TaskSpec{
			{Kind: &gocdnextv1.TaskSpec_Script{Script: "echo 1"}},
			{Kind: &gocdnextv1.TaskSpec_Script{Script: "echo 2"}},
		},
	}
	r.Execute(context.Background(), a)

	rc.mu.Lock()
	defer rc.mu.Unlock()
	if got := len(rc.results); got != 1 {
		t.Fatalf("want 1 JobResult, got %d", got)
	}
	if got := rc.results[0].GetStatus(); got != gocdnextv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("status: want FAILED, got %v", got)
	}
	if !contains(rc.logs, "exactly one task") {
		t.Errorf("expected multi-task rejection log; got %v", rc.logs)
	}
}

func TestExecute_Isolated_PropagatesFirstCheckoutTargetDirToWorkDir(t *testing.T) {
	// Regression for v0.5.0 → v0.5.1: in isolated mode the task
	// container's WorkingDir was hardcoded to WorkspaceMountPath
	// (e.g. /workspace), while prep cloned the primary material
	// into /workspace/<target_dir>. The plugin then ran at
	// /workspace, saw no lockfile, and exited 2. Drive a
	// best-effort end-to-end: any IsolatedJobSpec we build for an
	// assignment whose first checkout has a non-empty target_dir
	// MUST set WorkDir = workspaceMountPath + target_dir.
	//
	// We can't easily run the full executeIsolated against the
	// fake clientset (Pod state polling), so the assertion targets
	// the path-derivation logic via a stripped-down helper exposed
	// for tests below.
	got := resolveIsolatedScriptWorkDir("/workspace", []*gocdnextv1.MaterialCheckout{
		{Url: "https://example/git", TargetDir: "src/main"},
	})
	if want := "/workspace/src/main"; got != want {
		t.Errorf("workdir for target_dir=src/main: want %q, got %q", want, got)
	}

	got = resolveIsolatedScriptWorkDir("/workspace", nil)
	if want := "/workspace"; got != want {
		t.Errorf("workdir for no checkouts: want %q, got %q", want, got)
	}

	got = resolveIsolatedScriptWorkDir("/workspace", []*gocdnextv1.MaterialCheckout{
		{Url: "https://example/git", TargetDir: ""},
	})
	if want := "/workspace"; got != want {
		t.Errorf("workdir for empty target_dir: want %q, got %q", want, got)
	}
}

// TestCleanupIsolatedPod_KeepsPodWhenCleanupDisabled covers the
// pre-existing behaviour: a job that failed naturally (non-zero
// task exit, prep crash, etc.) keeps its pod around for debugging
// when CleanupOnFailure=false. The cancel-path regression below
// only fires for context.Canceled — natural failures must NOT
// take the override path.
func TestCleanupIsolatedPod_KeepsPodWhenCleanupDisabled(t *testing.T) {
	cli := fake.NewSimpleClientset()
	k := engine.NewKubernetesWithClient(cli, engine.KubernetesConfig{
		Namespace:        "ci",
		WorkspaceMode:    engine.WorkspaceModeIsolated,
		CleanupOnFailure: false,
		CleanupOnSuccess: true, // also exercise the success/keep half
	})
	ctx := context.Background()
	mustCreatePod(t, ctx, cli, "ci", "job-pod-keep")

	r := New(Config{
		Engine: k,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r.cleanupIsolatedPod(ctx, k, "job-pod-keep", false /* success */)

	if _, err := cli.CoreV1().Pods("ci").Get(ctx, "job-pod-keep", metav1.GetOptions{}); err != nil {
		t.Errorf("pod should still exist for debugging; got err=%v", err)
	}
}

// TestCleanupIsolatedPod_DeletesOnCancelDespiteCleanupOnFailureFalse
// is the regression for the v0.14.2 cancel-doesn't-kill-pod bug.
// Before the fix, Runner.Cancel canceled the job ctx but the
// cleanup path then took the !CleanupOnFailure branch and the pod
// stayed Running until an operator nuked it by hand. After the
// fix, a canceled ctx forces the delete regardless of the
// cleanup-on-failure policy — "Cancel" actually cancels.
func TestCleanupIsolatedPod_DeletesOnCancelDespiteCleanupOnFailureFalse(t *testing.T) {
	cli := fake.NewSimpleClientset()
	k := engine.NewKubernetesWithClient(cli, engine.KubernetesConfig{
		Namespace:        "ci",
		WorkspaceMode:    engine.WorkspaceModeIsolated,
		CleanupOnFailure: false, // would normally KEEP the pod
	})
	ctx, cancel := context.WithCancel(context.Background())
	mustCreatePod(t, ctx, cli, "ci", "job-pod-cancel")

	// Simulate the Runner.Cancel call: the job's ctx is canceled.
	cancel()

	r := New(Config{
		Engine: k,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r.cleanupIsolatedPod(ctx, k, "job-pod-cancel", false /* success */)

	// Pod MUST be gone — cancellation overrides CleanupOnFailure=false.
	if _, err := cli.CoreV1().Pods("ci").Get(context.Background(), "job-pod-cancel", metav1.GetOptions{}); err == nil {
		t.Errorf("pod %q should have been deleted on cancel despite CleanupOnFailure=false", "job-pod-cancel")
	}
}

func mustCreatePod(t *testing.T, ctx context.Context, cli *fake.Clientset, ns, name string) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if _, err := cli.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod %s: %v", name, err)
	}
}

func contains(ss []string, sub string) bool {
	for _, s := range ss {
		if len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) {
			return true
		}
	}
	return false
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
