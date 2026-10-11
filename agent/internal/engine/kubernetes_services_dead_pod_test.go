package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// This file covers the adoption path: a run-scoped service pod that a
// SIBLING job already created. `EnsureServices` gets AlreadyExists,
// Gets the pod, validates it is ours, and reuses it.
//
// The bug: Kubernetes keeps `status.podIP` populated on a terminal pod
// until the object is deleted, and waitForPodIP checked podIP BEFORE
// the phase. So a service that had served and then died was adopted as
// `ready` — the agent emitted a ready lifecycle event, wired the dead
// IP into the job's hostAliases, and the job failed later with
// `connection refused` in whatever code happened to touch the service.
// Same user-visible shape as #305: the control plane says the service
// is fine, the job looks broken.

// putServicePod seeds a pod carrying the full label set
// assertOurServicePod requires, so EnsureServices takes the reuse path.
func putServicePod(t *testing.T, cli *fake.Clientset, ns, runID, svc string, status corev1.PodStatus) string {
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
			Containers: []corev1.Container{{Name: "service", Image: "postgres:17-alpine"}},
		},
		Status: status,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := cli.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed service pod: %v", err)
	}
	return name
}

// deadPodStatus is what kubelet leaves behind when a service that WAS
// serving dies: terminal phase, terminated container — and the podIP
// still there.
func deadPodStatus(phase corev1.PodPhase, exit int32, reason string) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: phase,
		PodIP: "10.1.2.3",
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "service",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: exit,
				Reason:   reason,
			}},
		}},
	}
}

func livePodStatus() corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		PodIP: "10.1.2.3",
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "service",
			Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
}

type lifecycleRecorder struct {
	mu     sync.Mutex
	events []engine.ServiceLifecycleEvent
}

func (r *lifecycleRecorder) record(evt engine.ServiceLifecycleEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, evt)
}

func (r *lifecycleRecorder) statuses() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Status)
	}
	return out
}

func (r *lifecycleRecorder) withStatus(status string) []engine.ServiceLifecycleEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []engine.ServiceLifecycleEvent
	for _, e := range r.events {
		if e.Status == status {
			out = append(out, e)
		}
	}
	return out
}

type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) log(stream, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, stream+": "+text)
}

func (l *logRecorder) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func TestEnsureServices_RefusesToAdoptADeadServicePod(t *testing.T) {
	tests := []struct {
		name   string
		status corev1.PodStatus
		wantIn []string
	}{
		{
			name:   "failed with a stale podIP",
			status: deadPodStatus(corev1.PodFailed, 137, "OOMKilled"),
			wantIn: []string{"postgres", "exit 137", "OOMKilled"},
		},
		{
			name:   "succeeded with a stale podIP",
			status: deadPodStatus(corev1.PodSucceeded, 0, "Completed"),
			wantIn: []string{"postgres", "exit 0"},
		},
		{
			// Phase lags container status by a poll.
			name: "container terminated while the phase still says Running",
			status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.1.2.3",
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "service",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
				}},
			},
			wantIn: []string{"postgres", "exit 1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, cli := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
			const runID = "rundeadsvc01"
			podName := putServicePod(t, cli, "gocdnext-tests", runID, "postgres", tt.status)

			lc := &lifecycleRecorder{}
			lg := &logRecorder{}

			_, err := k.EnsureServices(context.Background(),
				[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:17-alpine"}},
				runID, "job-2", 0, lg.log, lc.record)
			if err == nil {
				t.Fatal("adopted a dead service pod as ready — the job would have run against a stale podIP and failed with connection refused")
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
			if !strings.Contains(err.Error(), podName) {
				t.Errorf("error %q does not name the pod", err)
			}

			// The control plane must not be told the service is ready.
			// That `ready` was the lie that made #305-shaped incidents
			// unreadable from the UI.
			for _, s := range lc.statuses() {
				if s == "ready" {
					t.Errorf("emitted a ready lifecycle event for a dead service; events = %v", lc.statuses())
				}
			}

			// Not emitting `ready` is only half of it: the server moves
			// a service_runs row ONLY on a ServiceLifecycle event, so
			// without an explicit `failed` the row keeps whatever the
			// creator last reported — `ready` — until cleanup stamps
			// `stopped`. The service would render green while this job
			// failed because of it.
			failed := lc.withStatus("failed")
			if len(failed) != 1 {
				t.Fatalf("want exactly 1 failed event, got %d (events = %v)", len(failed), lc.statuses())
			}
			evt := failed[0]
			if evt.Name != "postgres" {
				t.Errorf("failed event Name = %q, want postgres", evt.Name)
			}
			if evt.Image != "postgres:17-alpine" {
				t.Errorf("failed event Image = %q, want the declared image", evt.Image)
			}
			if evt.PodName != podName {
				t.Errorf("failed event PodName = %q, want %q", evt.PodName, podName)
			}
			// The row's error renders in the service popover, so it has
			// to carry the cause, not just "it failed".
			for _, want := range tt.wantIn {
				if want == "postgres" {
					continue // service name, asserted on Name above
				}
				if !strings.Contains(evt.Error, want) {
					t.Errorf("failed event Error = %q, missing %q", evt.Error, want)
				}
			}

			// And the job log must not claim a successful reuse.
			if got := lg.joined(); strings.Contains(got, "reusing service postgres") {
				t.Errorf("logged a successful reuse of a dead pod:\n%s", got)
			}
		})
	}
}

// TestEnsureServices_StillAdoptsALiveServicePod is the regression guard
// for the common path: the whole point of run-scoped services is that
// sibling jobs reuse them.
func TestEnsureServices_StillAdoptsALiveServicePod(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	const runID = "runlivesvc01"
	putServicePod(t, cli, "gocdnext-tests", runID, "postgres", livePodStatus())

	lc := &lifecycleRecorder{}
	lg := &logRecorder{}

	wireup, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:17-alpine"}},
		runID, "job-2", 0, lg.log, lc.record)
	if err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}
	if len(wireup.HostAliases) != 1 || wireup.HostAliases[0].IP != "10.1.2.3" {
		t.Errorf("host aliases = %+v, want the live service IP", wireup.HostAliases)
	}
	if !strings.Contains(lg.joined(), "reusing service postgres") {
		t.Errorf("expected the reuse log line; got:\n%s", lg.joined())
	}
	var sawReady bool
	for _, s := range lc.statuses() {
		if s == "ready" {
			sawReady = true
		}
	}
	if !sawReady {
		t.Errorf("no ready event for a live reused service; events = %v", lc.statuses())
	}
	if got := lc.withStatus("failed"); len(got) != 0 {
		t.Errorf("emitted %d failed event(s) for a live service: %+v", len(got), got)
	}
}

// TestEnsureServices_AdoptsAPodThatIsStillStarting: a sibling job
// normally arrives while the creator's pod is still ContainerCreating.
// That is not death — it must wait for the IP like the creator does,
// not fail the job.
func TestEnsureServices_AdoptsAPodThatIsStillStarting(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	const runID = "runstartsvc1"
	name := putServicePod(t, cli, "gocdnext-tests", runID, "postgres", corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "service",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}},
	})
	// kubelet assigns the IP a moment later, as it would in a cluster.
	assignPodIPAsync(t, cli, "gocdnext-tests", name, "10.1.2.9", 20*time.Millisecond)

	wireup, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:17-alpine"}},
		runID, "job-2", 0, nil, nil)
	if err != nil {
		t.Fatalf("EnsureServices refused a service that was merely starting: %v", err)
	}
	if len(wireup.HostAliases) != 1 || wireup.HostAliases[0].IP != "10.1.2.9" {
		t.Errorf("host aliases = %+v, want the IP assigned after the wait", wireup.HostAliases)
	}
}

// TestEnsureServices_CreatorPathReportsDeathBeforeIP keeps the original
// startup-failure message meaningful: when the pod never got an IP, the
// error still says so rather than reporting a stale-IP adoption.
func TestEnsureServices_CreatorPathReportsDeathBeforeIP(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	const runID = "runnoipsvc01"
	putServicePod(t, cli, "gocdnext-tests", runID, "postgres", corev1.PodStatus{
		Phase: corev1.PodFailed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "service",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}},
	})

	_, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:17-alpine"}},
		runID, "job-2", 0, nil, nil)
	if err == nil {
		t.Fatal("expected an error for a pod that died without ever getting an IP")
	}
	if !strings.Contains(err.Error(), "exit 1") {
		t.Errorf("error %q should carry the exit code", err)
	}
}
