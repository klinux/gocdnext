package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// servicePod builds the shape CleanupRunServices actually sees: one
// "service" container, the status filled in by kubelet.
func servicePod(status corev1.ContainerStatus, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gocdnext-svc-abc-g0-postgres", Namespace: "ci"},
		Spec: corev1.PodSpec{
			NodeName:   "node-7",
			Containers: []corev1.Container{{Name: "service", Image: "postgres:17-alpine"}},
		},
		Status: corev1.PodStatus{
			Phase:             phase,
			ContainerStatuses: []corev1.ContainerStatus{status},
		},
	}
}

func TestServiceDiagnosticsFromPod_HealthyRunning(t *testing.T) {
	d := serviceDiagnosticsFromPod(servicePod(corev1.ContainerStatus{
		Name:  "service",
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
			StartedAt: metav1.NewTime(time.Unix(1700000000, 0)),
		}},
	}, corev1.PodRunning))

	if d == nil {
		t.Fatal("nil diagnostics")
	}
	if d.Phase != "Running" {
		t.Errorf("phase = %q", d.Phase)
	}
	if d.NodeName != "node-7" {
		t.Errorf("node = %q", d.NodeName)
	}
	if len(d.Containers) != 1 {
		t.Fatalf("containers = %d", len(d.Containers))
	}
	c := d.Containers[0]
	if c.State != "running" || !c.Ready || c.RestartCount != 0 {
		t.Errorf("container = %+v, want a ready running container with no restarts", c)
	}
	if c.LastTermination != nil {
		t.Errorf("unexpected last termination: %+v", c.LastTermination)
	}
	if d.Unhealthy() {
		t.Error("a ready, running, never-restarted service must not read as unhealthy")
	}
}

// TestServiceDiagnosticsFromPod_RestartedWithOOMKill is the signal
// #305 went looking for and could not find: a service that died and
// came back between `ready` and `stopped`.
func TestServiceDiagnosticsFromPod_RestartedWithOOMKill(t *testing.T) {
	started := metav1.NewTime(time.Unix(1700000000, 0))
	finished := metav1.NewTime(time.Unix(1700000300, 0))
	d := serviceDiagnosticsFromPod(servicePod(corev1.ContainerStatus{
		Name:         "service",
		Ready:        true,
		RestartCount: 1,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
			StartedAt: finished,
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason:     "OOMKilled",
			ExitCode:   137,
			Signal:     9,
			StartedAt:  started,
			FinishedAt: finished,
		}},
	}, corev1.PodRunning))

	c := d.Containers[0]
	if c.RestartCount != 1 {
		t.Errorf("restart count = %d, want 1", c.RestartCount)
	}
	if c.LastTermination == nil {
		t.Fatal("no last termination recorded")
	}
	if c.LastTermination.Reason != "OOMKilled" || c.LastTermination.ExitCode != 137 || c.LastTermination.Signal != 9 {
		t.Errorf("last termination = %+v", c.LastTermination)
	}
	if !c.LastTermination.FinishedAt.Equal(finished.Time) {
		t.Errorf("finished at = %v, want %v", c.LastTermination.FinishedAt, finished.Time)
	}
	if !d.Unhealthy() {
		t.Error("a restarted service must read as unhealthy")
	}
	if got := d.Summary(); !strings.Contains(got, "restarted 1") || !strings.Contains(got, "OOMKilled") {
		t.Errorf("summary = %q, want it to name the restart count and the reason", got)
	}
}

// TestServiceDiagnosticsFromPod_TerminatedCleanlyIsStillUnhealthy: a
// run-scoped service that exits mid-run — even with exit 0 — was not
// there for the jobs that came after it. That is the #305 failure
// shape, so it must not read as "fine".
func TestServiceDiagnosticsFromPod_TerminatedCleanlyIsStillUnhealthy(t *testing.T) {
	d := serviceDiagnosticsFromPod(servicePod(corev1.ContainerStatus{
		Name:  "service",
		Ready: false,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason:     "Completed",
			ExitCode:   0,
			FinishedAt: metav1.NewTime(time.Unix(1700000300, 0)),
		}},
	}, corev1.PodSucceeded))

	if !d.Unhealthy() {
		t.Error("a service that exited mid-run must read as unhealthy")
	}
	if got := d.Containers[0].State; got != "terminated" {
		t.Errorf("state = %q", got)
	}
}

func TestServiceDiagnosticsFromPod_WaitingWithReason(t *testing.T) {
	d := serviceDiagnosticsFromPod(servicePod(corev1.ContainerStatus{
		Name: "service",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason:  "CrashLoopBackOff",
			Message: "back-off 5m0s restarting failed container",
		}},
		RestartCount: 5,
	}, corev1.PodRunning))

	c := d.Containers[0]
	if c.State != "waiting" || c.StateReason != "CrashLoopBackOff" {
		t.Errorf("container = %+v", c)
	}
	if !d.Unhealthy() {
		t.Error("CrashLoopBackOff must read as unhealthy")
	}
}

// TestServiceDiagnosticsFromPod_RecordsMissingResources is the
// actionable half for #305: the service pod is built with NO resources
// at all, so its memory ceiling is the node's. The snapshot has to say
// so explicitly, otherwise "no limit" is indistinguishable from
// "didn't look".
func TestServiceDiagnosticsFromPod_RecordsMissingResources(t *testing.T) {
	d := serviceDiagnosticsFromPod(servicePod(corev1.ContainerStatus{
		Name:  "service",
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}, corev1.PodRunning))

	c := d.Containers[0]
	if c.MemLimit != "" || c.CPULimit != "" || c.MemRequest != "" || c.CPURequest != "" {
		t.Errorf("resources = %+v, want all empty for a pod built without any", c)
	}
	attrs := attrMap(t, d.LogAttrs())
	if got := attrs["svc_mem_limit"]; got != "(none)" {
		t.Errorf("svc_mem_limit = %v, want the explicit \"(none)\"", got)
	}
}

func TestServiceDiagnosticsFromPod_CarriesDeclaredResources(t *testing.T) {
	pod := servicePod(corev1.ContainerStatus{
		Name:  "service",
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}, corev1.PodRunning)
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}

	c := serviceDiagnosticsFromPod(pod).Containers[0]
	if c.CPURequest != "100m" || c.MemRequest != "256Mi" || c.MemLimit != "1Gi" {
		t.Errorf("resources = %+v", c)
	}
	if c.CPULimit != "" {
		t.Errorf("cpu limit = %q, want empty", c.CPULimit)
	}
}

func TestServiceDiagnosticsFromPod_PodLevelReasonAndConditions(t *testing.T) {
	pod := servicePod(corev1.ContainerStatus{
		Name:  "service",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}},
	}, corev1.PodFailed)
	pod.Status.Reason = "Evicted"
	pod.Status.Message = "The node was low on resource: memory."
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"},
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
	}

	d := serviceDiagnosticsFromPod(pod)
	if d.Reason != "Evicted" {
		t.Errorf("pod-level reason = %q", d.Reason)
	}
	if len(d.Conditions) != 2 {
		t.Fatalf("conditions = %+v", d.Conditions)
	}
	if d.Conditions[0].Type != "Ready" || d.Conditions[0].Status != "False" || d.Conditions[0].Reason != "ContainersNotReady" {
		t.Errorf("condition[0] = %+v", d.Conditions[0])
	}
	if !d.Unhealthy() {
		t.Error("an evicted pod must read as unhealthy")
	}
	if got := d.Summary(); !strings.Contains(got, "Evicted") {
		t.Errorf("summary = %q, want it to name the eviction", got)
	}
}

func TestServiceDiagnosticsFromPod_NilPod(t *testing.T) {
	if d := serviceDiagnosticsFromPod(nil); d != nil {
		t.Errorf("want nil for a nil pod, got %+v", d)
	}
}

// TestServiceDiagnostics_LogAttrsPairs guards the contract the rpc
// client relies on: an even-length key/value run that splats straight
// into slog. An odd tail would make slog emit a "!BADKEY" record.
func TestServiceDiagnostics_LogAttrsPairs(t *testing.T) {
	d := serviceDiagnosticsFromPod(servicePod(corev1.ContainerStatus{
		Name:         "service",
		RestartCount: 2,
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", ExitCode: 1,
		}},
	}, corev1.PodRunning))

	attrs := d.LogAttrs()
	if len(attrs)%2 != 0 {
		t.Fatalf("LogAttrs returned %d values, want an even key/value run: %v", len(attrs), attrs)
	}
	for i := 0; i < len(attrs); i += 2 {
		if _, ok := attrs[i].(string); !ok {
			t.Errorf("attrs[%d] = %v, want a string key", i, attrs[i])
		}
	}
	m := attrMap(t, attrs)
	for _, key := range []string{
		"svc_pod", "svc_phase", "svc_node", "svc_restarts",
		"svc_state", "svc_last_terminated", "svc_last_exit_code",
		"svc_last_reason", "svc_mem_limit",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing attr %q in %v", key, m)
		}
	}
	if got := m["svc_restarts"]; got != int32(2) {
		t.Errorf("svc_restarts = %#v, want int32(2)", got)
	}
}

// TestServiceDiagnostics_NilReceiverIsSafe: the field is a pointer on
// ServiceLifecycleEvent and every non-cleanup emitter leaves it nil,
// so the consumers must tolerate that without a guard at each site.
func TestServiceDiagnostics_NilReceiverIsSafe(t *testing.T) {
	var d *ServicePodDiagnostics
	if d.Unhealthy() {
		t.Error("nil diagnostics must not read as unhealthy")
	}
	if got := d.Summary(); got != "" {
		t.Errorf("Summary() = %q, want empty", got)
	}
	if got := d.LogAttrs(); got != nil {
		t.Errorf("LogAttrs() = %v, want nil", got)
	}
}

// TestServiceDiagnostics_NeverCarriesWorkloadText is the guard for the
// exfiltration path a reviewer caught before this shipped.
//
// ContainerStateTerminated.Message is read from the container's
// /dev/termination-log — the WORKLOAD writes it. This snapshot is
// logged by the agent, and the agent's own log does NOT pass through
// the job's LogMasks, so any text carried here would let a pipeline
// push arbitrary bytes (a resolved secret included) into the
// operator's log pipeline, unredacted and under a different retention
// policy than the job log.
//
// The rule is blanket — no Message anywhere, pod/condition/container —
// because "safe while waiting, attacker-controlled once terminated" is
// not a distinction that survives maintenance.
func TestServiceDiagnostics_NeverCarriesWorkloadText(t *testing.T) {
	const secret = "SUPERSECRET_ghp_abc123_from_termination_log"

	pod := servicePod(corev1.ContainerStatus{
		Name:         "service",
		RestartCount: 1,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason:  "CrashLoopBackOff",
			Message: secret,
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason:   "Error",
			ExitCode: 1,
			Message:  secret,
		}},
	}, corev1.PodFailed)
	pod.Status.Message = secret
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodReady,
		Status:  corev1.ConditionFalse,
		Reason:  "ContainersNotReady",
		Message: secret,
	}}

	d := serviceDiagnosticsFromPod(pod)

	// 1. Not in the struct at all — so no future consumer (the service
	//    record, a metric label, an API field) can reintroduce the leak.
	if blob := fmt.Sprintf("%+v", *d); strings.Contains(blob, secret) {
		t.Error("the snapshot struct retains workload-authored text")
	}
	for _, c := range d.Containers {
		if blob := fmt.Sprintf("%+v", c); strings.Contains(blob, secret) {
			t.Error("the container snapshot retains workload-authored text")
		}
		if c.LastTermination != nil {
			if blob := fmt.Sprintf("%+v", *c.LastTermination); strings.Contains(blob, secret) {
				t.Error("the termination snapshot retains workload-authored text")
			}
		}
	}
	for _, cond := range d.Conditions {
		if blob := fmt.Sprintf("%+v", cond); strings.Contains(blob, secret) {
			t.Error("the condition snapshot retains workload-authored text")
		}
	}

	// 2. Not in the slog attrs.
	for i, v := range d.LogAttrs() {
		if str, ok := v.(string); ok && strings.Contains(str, secret) {
			t.Errorf("LogAttrs[%d] leaks workload-authored text", i)
		}
	}

	// 3. Not in the human summary, which also reaches the job log.
	if strings.Contains(d.Summary(), secret) {
		t.Error("Summary() leaks workload-authored text")
	}

	// Sanity: the bounded fields ARE still reported, so this test can't
	// pass by the extraction having silently become a no-op.
	if d.Containers[0].StateReason != "CrashLoopBackOff" {
		t.Errorf("state reason = %q, want it kept", d.Containers[0].StateReason)
	}
	if d.Containers[0].LastTermination.Reason != "Error" {
		t.Errorf("termination reason = %q, want it kept", d.Containers[0].LastTermination.Reason)
	}
}

// TestServiceDiagnostics_LogAttrTypesAreStable: a structured-log
// backend infers one type per field, so a key that is an int on one
// record and a string on another becomes unqueryable (or is rejected by
// a strict index). svc_last_terminated carries the "did it ever die"
// bit so the exit-code keys never need a sentinel.
func TestServiceDiagnostics_LogAttrTypesAreStable(t *testing.T) {
	never := attrMap(t, serviceDiagnosticsFromPod(servicePod(corev1.ContainerStatus{
		Name:  "service",
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}, corev1.PodRunning)).LogAttrs())

	died := attrMap(t, serviceDiagnosticsFromPod(servicePod(corev1.ContainerStatus{
		Name:         "service",
		RestartCount: 1,
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "OOMKilled", ExitCode: 137, Signal: 9,
		}},
	}, corev1.PodRunning)).LogAttrs())

	if got := never["svc_last_terminated"]; got != false {
		t.Errorf("svc_last_terminated = %#v, want false", got)
	}
	if _, present := never["svc_last_exit_code"]; present {
		t.Error("svc_last_exit_code present for a container that never died — it would need a sentinel value")
	}
	if got := died["svc_last_terminated"]; got != true {
		t.Errorf("svc_last_terminated = %#v, want true", got)
	}
	if got := died["svc_last_exit_code"]; got != int32(137) {
		t.Errorf("svc_last_exit_code = %#v, want int32(137)", got)
	}

	// Every key present in both records must hold the same Go type.
	for key, a := range never {
		b, ok := died[key]
		if !ok {
			continue
		}
		if fmt.Sprintf("%T", a) != fmt.Sprintf("%T", b) {
			t.Errorf("key %q changes type across records: %T vs %T", key, a, b)
		}
	}
}

func attrMap(t *testing.T, attrs []any) map[string]any {
	t.Helper()
	if len(attrs)%2 != 0 {
		t.Fatalf("odd attr run: %v", attrs)
	}
	out := make(map[string]any, len(attrs)/2)
	for i := 0; i < len(attrs); i += 2 {
		key, ok := attrs[i].(string)
		if !ok {
			t.Fatalf("attrs[%d] is not a string key: %v", i, attrs[i])
		}
		out[key] = attrs[i+1]
	}
	return out
}
