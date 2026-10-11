package engine_test

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// seedServicePod creates a service pod carrying the labels
// CleanupRunServices selects on, with a status that says it died and
// came back once — the #305 signal.
func seedServicePod(t *testing.T, cli *fake.Clientset, ns, runID, svc string, status corev1.ContainerStatus) string {
	t.Helper()
	name := "gocdnext-svc-" + runID[:12] + "-g0-" + svc
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":         "gocdnext-service",
				"app.kubernetes.io/component":    "service",
				"app.kubernetes.io/managed-by":   "gocdnext-agent",
				"gocdnext.io/service":            svc,
				"gocdnext.io/run-id":             runID,
				"gocdnext.io/service-generation": "0",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-7",
			Containers: []corev1.Container{{Name: "service", Image: "postgres:17-alpine"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{status},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := cli.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed service pod: %v", err)
	}
	return name
}

func restartedStatus() corev1.ContainerStatus {
	started := metav1.NewTime(time.Unix(1700000000, 0))
	finished := metav1.NewTime(time.Unix(1700000300, 0))
	return corev1.ContainerStatus{
		Name:         "service",
		Ready:        true,
		RestartCount: 1,
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: finished}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason:     "OOMKilled",
			ExitCode:   137,
			Signal:     9,
			StartedAt:  started,
			FinishedAt: finished,
		}},
	}
}

func healthyStatus() corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  "service",
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
			StartedAt: metav1.NewTime(time.Unix(1700000000, 0)),
		}},
	}
}

// TestCleanupRunServices_StoppedEventCarriesDiagnostics is the core of
// the #305 fix: the run-terminal teardown already Lists the pods with
// their status, so the "it restarted mid-run" evidence is free. Before
// this, the `stopped` event carried nothing and the incident was
// invisible outside the cluster.
func TestCleanupRunServices_StoppedEventCarriesDiagnostics(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	const runID = "runxyz12abcd"
	podName := seedServicePod(t, cli, "gocdnext-tests", runID, "postgres", restartedStatus())

	var (
		mu     sync.Mutex
		events []engine.ServiceLifecycleEvent
	)
	deleted, err := k.CleanupRunServices(context.Background(), runID, math.MaxInt64,
		func(evt engine.ServiceLifecycleEvent) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, evt)
		})
	if err != nil {
		t.Fatalf("CleanupRunServices: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 stopped event", len(events))
	}
	evt := events[0]
	if evt.Status != "stopped" {
		t.Errorf("status = %q", evt.Status)
	}
	d := evt.Diagnostics
	if d == nil {
		t.Fatal("stopped event carries no diagnostics — the #305 blind spot is back")
	}
	if d.PodName != podName {
		t.Errorf("pod = %q, want %q", d.PodName, podName)
	}
	if !d.Unhealthy() {
		t.Error("a service that restarted must read as unhealthy")
	}
	if len(d.Containers) != 1 {
		t.Fatalf("containers = %+v", d.Containers)
	}
	c := d.Containers[0]
	if c.RestartCount != 1 {
		t.Errorf("restarts = %d, want 1", c.RestartCount)
	}
	if c.LastTermination == nil || c.LastTermination.Reason != "OOMKilled" || c.LastTermination.ExitCode != 137 {
		t.Errorf("last termination = %+v, want the OOMKill", c.LastTermination)
	}
	// The actionable half: this pod was built with no resources at all.
	if c.MemLimit != "" {
		t.Errorf("mem limit = %q, want empty for a pod built without resources", c.MemLimit)
	}
	if got := d.Summary(); !strings.Contains(got, "restarted 1") {
		t.Errorf("summary = %q", got)
	}
}

// TestCleanupRunServices_HealthyServiceStillReportsSnapshot: the
// snapshot is unconditional so a reader can tell "observed, fine" from
// "not observed". Only the caller's log LEVEL depends on Unhealthy.
func TestCleanupRunServices_HealthyServiceStillReportsSnapshot(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	const runID = "runhealthy01"
	seedServicePod(t, cli, "gocdnext-tests", runID, "redis", healthyStatus())

	var got *engine.ServicePodDiagnostics
	if _, err := k.CleanupRunServices(context.Background(), runID, math.MaxInt64,
		func(evt engine.ServiceLifecycleEvent) { got = evt.Diagnostics }); err != nil {
		t.Fatalf("CleanupRunServices: %v", err)
	}
	if got == nil {
		t.Fatal("no diagnostics for a healthy service")
	}
	if got.Unhealthy() {
		t.Errorf("healthy service read as unhealthy: %+v", got)
	}
	if got.NodeName != "node-7" {
		t.Errorf("node = %q", got.NodeName)
	}
}

// TestCleanupRunServices_NilLifecycleStillDeletes keeps the snapshot
// off the critical path: the rpc client passes nil on some paths, and
// teardown must not care.
func TestCleanupRunServices_NilLifecycleStillDeletes(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	const runID = "runnolifecy1"
	seedServicePod(t, cli, "gocdnext-tests", runID, "postgres", restartedStatus())

	deleted, err := k.CleanupRunServices(context.Background(), runID, math.MaxInt64, nil)
	if err != nil {
		t.Fatalf("CleanupRunServices: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
}

// TestEnsureServices_ReusePathWarnsOnRestartedService is the only
// surface that reaches a human DURING the run: a sibling job that
// reuses a run-scoped service already Gets the existing pod, so an
// earlier restart can be called out in that job's own log instead of
// being discovered later in a post-mortem.
func TestEnsureServices_ReusePathWarnsOnRestartedService(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	const runID = "runreuse0123"
	name := seedServicePod(t, cli, "gocdnext-tests", runID, "postgres", restartedStatus())
	// The reuse path waits for the podIP like the creator does.
	patchPodIP(t, cli, "gocdnext-tests", name, "10.1.2.3")

	var (
		mu    sync.Mutex
		lines []string
	)
	logf := func(stream, text string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, stream+": "+text)
	}

	if _, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:17-alpine"}},
		runID, "job-2", 0, logf, nil); err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "reusing service postgres") {
		t.Fatalf("expected the reuse log line; got %v", lines)
	}
	if !strings.Contains(joined, "restarted 1 time(s)") || !strings.Contains(joined, "OOMKilled") {
		t.Errorf("reuse path did not surface the restart to the job log; got %v", lines)
	}
	if !strings.Contains(joined, "stderr:") {
		t.Errorf("the warning should go to stderr so it stands out; got %v", lines)
	}
}

// TestEnsureServices_ReusePathQuietOnHealthyService keeps the common
// case unchanged: a dozen jobs reusing a healthy service must not each
// print a diagnostic line.
func TestEnsureServices_ReusePathQuietOnHealthyService(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	const runID = "runquiet0123"
	name := seedServicePod(t, cli, "gocdnext-tests", runID, "postgres", healthyStatus())
	patchPodIP(t, cli, "gocdnext-tests", name, "10.1.2.3")

	var lines []string
	logf := func(stream, text string) { lines = append(lines, stream+": "+text) }

	if _, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:17-alpine"}},
		runID, "job-2", 0, logf, nil); err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}

	for _, l := range lines {
		if strings.Contains(l, "restarted") || strings.Contains(l, "stderr:") {
			t.Errorf("healthy reuse produced a diagnostic line: %q", l)
		}
	}
}

// TestEnsureServices_ReusePathQuietWhileServiceIsStillStarting is the
// corner case that makes the reuse warning usable: sibling jobs
// normally arrive microseconds after the creator, with the pod still in
// ContainerCreating. That is not a fault — waitForPodIP handles
// startup — so warning there would train everyone to ignore the line.
func TestEnsureServices_ReusePathQuietWhileServiceIsStillStarting(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	const runID = "runstarting1"
	name := seedServicePod(t, cli, "gocdnext-tests", runID, "postgres", corev1.ContainerStatus{
		Name:  "service",
		Ready: false,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "ContainerCreating",
		}},
	})
	patchPodIP(t, cli, "gocdnext-tests", name, "10.1.2.3")

	var lines []string
	logf := func(stream, text string) { lines = append(lines, stream+": "+text) }

	if _, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:17-alpine"}},
		runID, "job-2", 0, logf, nil); err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}

	for _, l := range lines {
		if strings.Contains(l, "not healthy") {
			t.Errorf("warned about a service that is merely still starting: %q", l)
		}
	}
}

// TestServiceDiagnostics_DiedVsUnhealthy pins the split between the two
// predicates, since using the wrong one at either call site is either
// noise (reuse) or a missed signal (teardown).
func TestServiceDiagnostics_DiedVsUnhealthy(t *testing.T) {
	starting := &engine.ServicePodDiagnostics{
		Phase: "Pending",
		Containers: []engine.ServiceContainerDiagnostics{{
			Name: "service", State: "waiting", StateReason: "ContainerCreating",
		}},
	}
	if starting.Died() {
		t.Error("a starting container has not died")
	}
	if !starting.Unhealthy() {
		t.Error("at teardown, a non-running container is still a finding")
	}

	restarted := &engine.ServicePodDiagnostics{
		Phase: "Running",
		Containers: []engine.ServiceContainerDiagnostics{{
			Name: "service", State: "running", RestartCount: 1,
			LastTermination: &engine.ContainerTermination{Reason: "OOMKilled", ExitCode: 137},
		}},
	}
	if !restarted.Died() || !restarted.Unhealthy() {
		t.Error("a restarted container must trip both predicates")
	}

	var nilDiag *engine.ServicePodDiagnostics
	if nilDiag.Died() {
		t.Error("nil diagnostics must not report a death")
	}
}

func patchPodIP(t *testing.T, cli *fake.Clientset, ns, name, ip string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pod, err := cli.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	pod.Status.PodIP = ip
	if _, err := cli.CoreV1().Pods(ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("set podIP on %s: %v", name, err)
	}
}
