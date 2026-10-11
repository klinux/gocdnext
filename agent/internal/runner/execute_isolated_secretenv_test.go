package runner

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// noopExecutor satisfies the PodExecutor the isolated path requires.
// This test never reaches a post-task exec (the task container is held
// in Waiting), so the methods are unreachable in practice.
type noopExecutor struct{}

func (noopExecutor) Exec(context.Context, string, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}

// apiCalls records the API verbs the flow issues, so a test can
// assert on ORDER rather than only on end state.
type apiCalls struct {
	mu   sync.Mutex
	seen []string
}

func (a *apiCalls) add(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, s)
}

func (a *apiCalls) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func (a *apiCalls) indexOf(s string) int {
	for i, got := range a.snapshot() {
		if got == s {
			return i
		}
	}
	return -1
}

// TestExecute_Isolated_EnvSecretOutlivesPrep is the regression guard
// for the bug that the two-Secret split exists to prevent.
//
// The runner deletes the assignment Secret the instant prep
// terminates — deliberately, so the serialised JobAssignment (env,
// signed URLs, scripts) does not outlive the one container that needs
// it. The task container's secret-bearing env, however, is resolved by
// the kubelet LATER, when it creates that container. Serving both from
// one object therefore strands every secret-bearing isolated job in
// CreateContainerConfigError.
//
// Flow driven here: pod created → prep observed Terminated(0) →
// assignment Secret deleted → task container held in Waiting so
// WaitForTaskStarted times out and the job ends FAILED. By then the
// env Secret must still exist, and must never have been deleted.
func TestExecute_Isolated_EnvSecretOutlivesPrep(t *testing.T) {
	const secret = "ghp_abc123_never_inline"

	cli := fake.NewSimpleClientset()
	calls := &apiCalls{}
	var (
		podMu          sync.Mutex
		podName        string
		createdPodSpec *corev1.Pod
	)

	// Create assigns a UID (the fake clientset doesn't) so the
	// ownerRef patches land, and records the create order.
	cli.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod); ok {
			if pod.UID == "" {
				pod.UID = types.UID("uid-" + pod.Name)
			}
			podMu.Lock()
			podName = pod.Name
			createdPodSpec = pod.DeepCopy()
			podMu.Unlock()
		}
		calls.add("create:" + action.GetResource().Resource)
		return false, nil, nil
	})
	cli.PrependReactor("delete", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		calls.add("delete:secret:" + action.(k8stesting.DeleteAction).GetName())
		return false, nil, nil
	})

	// Pod status: prep Terminated(0) from the first poll, task
	// container stuck Waiting. WaitForInitStarted/Terminated pass
	// immediately; WaitForTaskStarted burns its StartupTimeout and
	// fails — which is fine, this test is about Secret lifetime, not
	// about a green job.
	cli.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		// `pods/log` arrives here too as a generic action — let it fall
		// through to the tracker (which serves canned log bytes)
		// instead of crashing on the GetAction assertion.
		get, ok := action.(k8stesting.GetAction)
		if !ok || action.GetSubresource() != "" {
			return false, nil, nil
		}
		name := get.GetName()
		calls.add("get:pod")
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ci", UID: types.UID("uid-" + name)},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				InitContainerStatuses: []corev1.ContainerStatus{{
					Name: "prep",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
					},
				}},
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "task",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"},
					},
				}},
			},
		}, nil
	})

	k := engine.NewKubernetesWithClient(cli, engine.KubernetesConfig{
		Namespace:          "ci",
		WorkspaceMode:      engine.WorkspaceModeIsolated,
		AgentImage:         "agent:v1",
		HousekeeperImage:   "alpine:3.19",
		DefaultImage:       "alpine:3.19",
		WorkspaceMountPath: "/workspace",
		WorkspaceSize:      "10Gi",
		PollInterval:       5 * time.Millisecond,
		StartupTimeout:     150 * time.Millisecond,
	})
	k.SetExecutor(noopExecutor{})

	rc := &resultCapture{}
	r := New(Config{
		WorkspaceRoot: t.TempDir(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send:          rc.send,
		Engine:        k,
	})

	r.Execute(context.Background(), &gocdnextv1.JobAssignment{
		RunId: "run-1", JobId: "job-1", Name: "build",
		Image:    "node:20",
		Env:      map[string]string{"GHCR_TOKEN": secret, "CI_BRANCH": "main"},
		LogMasks: []string{secret},
		Tasks: []*gocdnextv1.TaskSpec{{
			Kind: &gocdnextv1.TaskSpec_Script{Script: "pnpm test"},
		}},
	})

	ctx := context.Background()
	secrets, err := cli.CoreV1().Secrets("ci").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}

	// Secret names derive from the pod name the flow generated. The pod
	// itself is gone by now — the task-startup-timeout path
	// force-deletes it — so take the name from the create reactor.
	podMu.Lock()
	createdPod := podName
	podMu.Unlock()
	if createdPod == "" {
		t.Fatal("no pod was created")
	}

	// The invariant, stated without reference to any name: EVERY Secret
	// the task container resolves env from must still exist now that
	// prep is done — that is precisely the moment the kubelet would
	// create the container and look the keys up. This is what fails if
	// the env is ever served from the short-lived assignment Secret
	// again.
	podMu.Lock()
	spec := createdPodSpec
	podMu.Unlock()
	if spec == nil {
		t.Fatal("create reactor never saw a pod")
	}
	refCount := 0
	for _, c := range spec.Spec.Containers {
		if c.Name != "task" {
			continue
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				continue
			}
			refCount++
			ref := e.ValueFrom.SecretKeyRef
			got, gerr := cli.CoreV1().Secrets("ci").Get(ctx, ref.Name, metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("%s resolves from Secret %s/%s, which no longer exists after prep: %v (surviving: %v)",
					e.Name, ref.Name, ref.Key, gerr, secretNames(secrets))
			}
			if _, ok := got.Data[ref.Key]; !ok {
				t.Errorf("%s references key %s, absent from Secret %s", e.Name, ref.Key, ref.Name)
			}
		}
	}
	if refCount == 0 {
		t.Fatal("task container has no secretKeyRef env — the leak would be back and the assertions above vacuous")
	}

	envSecretName := engine.EnvSecretName(createdPod)
	envSecret, err := cli.CoreV1().Secrets("ci").Get(ctx, envSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("env secret %s must outlive prep — the task container's env resolves from it AFTER prep terminates: %v (surviving secrets: %v)",
			envSecretName, err, secretNames(secrets))
	}
	if got := string(envSecret.Data[engine.SecretEnvKey("GHCR_TOKEN")]); got != secret {
		t.Errorf("env secret data for GHCR_TOKEN = %q, want the token", got)
	}

	// The assignment Secret, by contrast, must be gone already.
	assignmentName := createdPod + "-assignment"
	if _, err := cli.CoreV1().Secrets("ci").Get(ctx, assignmentName, metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("assignment secret %s still present (err=%v) — it must not outlive prep", assignmentName, err)
	}

	// Order: the assignment delete happened, the env delete never did.
	for _, call := range calls.snapshot() {
		if call == "delete:secret:"+envSecretName {
			t.Error("runner deleted the env Secret — nothing may delete it but the pod's ownerRef GC")
		}
	}
	delIdx := calls.indexOf("delete:secret:" + assignmentName)
	if delIdx < 0 {
		t.Fatalf("assignment secret was never deleted; calls = %v", calls.snapshot())
	}
	// Both secrets are created before the pod (the pod's volume and
	// env refs point at them).
	for _, want := range []string{"create:secrets", "create:pods"} {
		if calls.indexOf(want) < 0 {
			t.Errorf("missing %s in %v", want, calls.snapshot())
		}
	}
	if calls.indexOf("create:pods") < calls.indexOf("create:secrets") {
		t.Errorf("pod created before its secrets; calls = %v", calls.snapshot())
	}
	if delIdx < calls.indexOf("create:pods") {
		t.Errorf("assignment secret deleted before the pod existed; calls = %v", calls.snapshot())
	}

	// Sanity: the job did run the flow to the task-start wait and
	// reported a result, rather than bailing out earlier for an
	// unrelated reason (which would make the assertions above vacuous).
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.results) != 1 {
		t.Fatalf("want 1 JobResult, got %d", len(rc.results))
	}
	if !contains(rc.logs, "task startup timeout") {
		t.Errorf("expected the flow to reach the task-start wait; logs = %v", rc.logs)
	}
	// The masked value must not survive into any log line either.
	for _, line := range rc.logs {
		if strings.Contains(line, secret) {
			t.Error("a log line carries the raw secret")
		}
	}
}

func secretNames(l *corev1.SecretList) []string {
	out := make([]string, 0, len(l.Items))
	for _, s := range l.Items {
		out = append(out, s.Name)
	}
	return out
}
