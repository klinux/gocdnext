package engine

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Service pods are bare Pods with no controller above them, their
// per-job Cleanup is a deliberate no-op, and nothing observes them
// between `ready` and the run-terminal delete. A service that died and
// came back mid-run was therefore indistinguishable, in the API and in
// the UI, from one that stayed healthy — the consumer job just failed
// with N errors in code that had nothing to do with the change under
// test (#305).
//
// This file closes the cheap half of that gap: the pod's own status
// already carries the evidence, and CleanupRunServices already Lists
// the pods (status included), so capturing it costs ZERO extra API
// calls and needs no new RBAC verb.
//
// Known blind spot, worth stating because it is the likely cause in the
// reported incident: if the node's OOM killer takes a CHILD process and
// the container's main process survives — a Postgres backend being
// killed while the postmaster recovers, which is what a
// 57P01 → 57P03 → recovery sequence inside one container looks like —
// then the container never restarted, RestartCount stays 0, and none of
// this reports anything. That signal lives in the node's SystemOOM
// event / kernel log, which the agent's Role cannot read today (it
// grants no `events`). The resource snapshot below is the actionable
// part for that case: it records that the service ran with no memory
// ceiling of its own at all.
//
// # No free text
//
// The snapshot carries ONLY bounded fields: kubelet's short Reason
// strings, exit code, signal, state, counts, node name, resource
// quantities. It deliberately carries no Message field anywhere, even
// though the Kubernetes API offers one at the pod, condition and
// container-state levels.
//
// Reason: a terminated container's Message is read from its
// /dev/termination-log, so its contents are chosen by the workload.
// This snapshot lands in the AGENT's operational log, which — unlike a
// job's log stream — does not pass through the job's LogMasks. Carrying
// that text would hand any pipeline a way to push arbitrary bytes, a
// resolved secret included, into the operator's log pipeline, where it
// persists under a different retention policy than the job log and is
// never redacted.
//
// Kubelet-authored messages (an eviction notice, a back-off
// explanation) would be safe in isolation, but "safe when the state is
// waiting, attacker-controlled when it is terminated" is not a rule
// that survives maintenance. One blanket rule does: no free text. The
// Reason field already names every case this telemetry is for
// (OOMKilled, Evicted, CrashLoopBackOff, Error), and an operator who
// needs the prose has `kubectl describe` with their own credentials.

// ContainerTermination is a flattened corev1.ContainerStateTerminated.
//
// Note the absence of Message — see the no-free-text rule in the
// package comment above. ContainerStateTerminated.Message is read from
// the container's /dev/termination-log, i.e. it is written by the
// workload, and this snapshot ends up in the agent's operational log,
// which does NOT pass through the job's LogMasks. Carrying it would
// hand any job a way to persist arbitrary bytes — a resolved secret
// included — into the operator's log pipeline.
type ContainerTermination struct {
	Reason     string
	ExitCode   int32
	Signal     int32
	StartedAt  time.Time
	FinishedAt time.Time
}

// ServiceContainerDiagnostics is the per-container slice of the
// snapshot. Resource strings are what the PodSpec ASKED for (empty
// when the field was never set) rather than a node-side effective
// value, which the agent cannot see.
type ServiceContainerDiagnostics struct {
	Name         string
	Ready        bool
	RestartCount int32

	// State is "running", "waiting", "terminated" or "unknown", and
	// StateReason carries kubelet's short explanation for the waiting
	// and terminated cases (ImagePullBackOff, CrashLoopBackOff,
	// OOMKilled, …). The matching Message is deliberately absent: for a
	// terminated container it comes from the workload's
	// /dev/termination-log.
	State       string
	StateReason string

	// LastTermination is non-nil only when the container died at least
	// once — i.e. exactly the "it went away and came back" case.
	LastTermination *ContainerTermination

	CPURequest string
	MemRequest string
	CPULimit   string
	MemLimit   string
}

// PodConditionSnapshot is a flattened corev1.PodCondition. Message is
// omitted under the same no-free-text rule.
type PodConditionSnapshot struct {
	Type   string
	Status string
	Reason string
}

// ServicePodDiagnostics is the whole snapshot, taken at teardown.
type ServicePodDiagnostics struct {
	PodName  string
	Phase    string
	Reason   string // pod-level, e.g. "Evicted"
	NodeName string

	Conditions []PodConditionSnapshot
	Containers []ServiceContainerDiagnostics
}

// serviceDiagnosticsFromPod flattens a Pod the engine already holds.
// nil in, nil out — callers treat a nil snapshot as "nothing observed"
// rather than branching.
func serviceDiagnosticsFromPod(pod *corev1.Pod) *ServicePodDiagnostics {
	if pod == nil {
		return nil
	}
	d := &ServicePodDiagnostics{
		PodName:  pod.Name,
		Phase:    string(pod.Status.Phase),
		Reason:   pod.Status.Reason,
		NodeName: pod.Spec.NodeName,
	}
	for _, c := range pod.Status.Conditions {
		d.Conditions = append(d.Conditions, PodConditionSnapshot{
			Type:   string(c.Type),
			Status: string(c.Status),
			Reason: c.Reason,
		})
	}

	// Requests/limits come from the SPEC, keyed by container name: the
	// status list and the spec list are not index-aligned in general
	// (status ordering is kubelet's).
	specByName := make(map[string]corev1.ResourceRequirements, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		specByName[c.Name] = c.Resources
	}

	for _, cs := range pod.Status.ContainerStatuses {
		cd := ServiceContainerDiagnostics{
			Name:         cs.Name,
			Ready:        cs.Ready,
			RestartCount: cs.RestartCount,
			State:        "unknown",
		}
		switch {
		case cs.State.Running != nil:
			cd.State = "running"
		case cs.State.Waiting != nil:
			cd.State = "waiting"
			cd.StateReason = cs.State.Waiting.Reason
		case cs.State.Terminated != nil:
			cd.State = "terminated"
			cd.StateReason = cs.State.Terminated.Reason
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			cd.LastTermination = &ContainerTermination{
				Reason:     t.Reason,
				ExitCode:   t.ExitCode,
				Signal:     t.Signal,
				StartedAt:  t.StartedAt.Time,
				FinishedAt: t.FinishedAt.Time,
			}
		}
		if res, ok := specByName[cs.Name]; ok {
			cd.CPURequest = quantityString(res.Requests, corev1.ResourceCPU)
			cd.MemRequest = quantityString(res.Requests, corev1.ResourceMemory)
			cd.CPULimit = quantityString(res.Limits, corev1.ResourceCPU)
			cd.MemLimit = quantityString(res.Limits, corev1.ResourceMemory)
		}
		d.Containers = append(d.Containers, cd)
	}
	return d
}

func quantityString(list corev1.ResourceList, name corev1.ResourceName) string {
	q, ok := list[name]
	if !ok {
		return ""
	}
	return q.String()
}

// Unhealthy reports whether the service was NOT continuously available
// for the whole run. True for: any restart, any recorded prior
// termination, a pod-level Failed phase, or a container that is not
// currently running.
//
// A clean `terminated` with exit 0 counts too, deliberately: a
// run-scoped service that exited mid-run was absent for every job that
// came after it, which is the #305 failure shape and not something to
// report as fine.
func (d *ServicePodDiagnostics) Unhealthy() bool {
	if d == nil {
		return false
	}
	if d.Phase == string(corev1.PodFailed) || d.Phase == string(corev1.PodSucceeded) {
		return true
	}
	for _, c := range d.Containers {
		if c.RestartCount > 0 || c.LastTermination != nil || c.State != "running" {
			return true
		}
	}
	return false
}

// Died reports the NARROW signal: the container actually went away at
// least once (or the pod reached a terminal phase). Unlike Unhealthy
// it ignores "not currently running", because a container that is
// merely starting has not failed at anything.
//
// This is the predicate for the mid-run reuse path: a sibling job
// typically arrives microseconds after the creator, with the pod still
// in ContainerCreating, and warning there would train everyone to
// ignore the line. At teardown the broader Unhealthy is the right
// question — by then the service should still be up.
func (d *ServicePodDiagnostics) Died() bool {
	if d == nil {
		return false
	}
	if d.Phase == string(corev1.PodFailed) || d.Phase == string(corev1.PodSucceeded) {
		return true
	}
	for _, c := range d.Containers {
		if c.RestartCount > 0 || c.LastTermination != nil {
			return true
		}
	}
	return false
}

// Summary is the one-line human form, for a job log line. Empty when
// there is nothing interesting (or nothing observed) — callers print it
// only when Unhealthy.
func (d *ServicePodDiagnostics) Summary() string {
	if d == nil {
		return ""
	}
	var parts []string
	if d.Reason != "" {
		parts = append(parts, d.Reason)
	}
	for _, c := range d.Containers {
		if c.RestartCount > 0 {
			parts = append(parts, fmt.Sprintf("restarted %d time(s)", c.RestartCount))
		}
		if c.LastTermination != nil {
			reason := c.LastTermination.Reason
			if reason == "" {
				reason = "unknown reason"
			}
			parts = append(parts, fmt.Sprintf("last exit %d (%s)", c.LastTermination.ExitCode, reason))
		}
		if c.State != "running" {
			state := c.State
			if c.StateReason != "" {
				state += ": " + c.StateReason
			}
			parts = append(parts, state)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

// LogAttrs returns an even key/value run for slog. The engine package
// stays logger-free (it reports through callbacks), so the caller
// splats this into its own logger.
//
// Two conventions worth knowing:
//
//   - Resource keys report "(none)" rather than "" when the field was
//     never set: "no memory ceiling" is a finding, and an empty string
//     reads as "we didn't look".
//   - Every key holds ONE type across every record. A structured-log
//     backend infers a type per field, so emitting svc_last_exit_code
//     as an int on one record and a sentinel string on another makes
//     the field unqueryable (or outright rejected, on a strict index).
//     svc_last_terminated is the boolean that says whether the
//     exit-code keys are present at all.
func (d *ServicePodDiagnostics) LogAttrs() []any {
	if d == nil {
		return nil
	}
	attrs := []any{
		"svc_pod", d.PodName,
		"svc_phase", d.Phase,
		"svc_node", d.NodeName,
	}
	if d.Reason != "" {
		attrs = append(attrs, "svc_pod_reason", d.Reason)
	}
	if cond, ok := d.condition(string(corev1.PodReady)); ok {
		attrs = append(attrs, "svc_ready_condition", cond.Status)
		if cond.Reason != "" {
			attrs = append(attrs, "svc_ready_reason", cond.Reason)
		}
	}

	// Service pods carry exactly one container ("service"); the loop is
	// defensive. Keys are unprefixed for the first container so the
	// common case reads cleanly, and suffixed beyond it rather than
	// silently overwriting.
	for i, c := range d.Containers {
		suffix := ""
		if i > 0 {
			suffix = fmt.Sprintf("_%d", i)
		}
		attrs = append(attrs,
			"svc_container"+suffix, c.Name,
			"svc_ready"+suffix, c.Ready,
			"svc_restarts"+suffix, c.RestartCount,
			"svc_state"+suffix, c.State,
			"svc_cpu_request"+suffix, orNone(c.CPURequest),
			"svc_mem_request"+suffix, orNone(c.MemRequest),
			"svc_cpu_limit"+suffix, orNone(c.CPULimit),
			"svc_mem_limit"+suffix, orNone(c.MemLimit),
		)
		if c.StateReason != "" {
			attrs = append(attrs, "svc_state_reason"+suffix, c.StateReason)
		}
		// svc_last_terminated is always present so a reader can tell
		// "never died" from "died with exit 0" without the exit-code
		// key having to carry a sentinel.
		t := c.LastTermination
		attrs = append(attrs, "svc_last_terminated"+suffix, t != nil)
		if t != nil {
			attrs = append(attrs,
				"svc_last_exit_code"+suffix, t.ExitCode,
				"svc_last_signal"+suffix, t.Signal,
				"svc_last_reason"+suffix, orNone(t.Reason),
				"svc_last_started_at"+suffix, timeString(t.StartedAt),
				"svc_last_finished_at"+suffix, timeString(t.FinishedAt),
			)
		}
	}
	return attrs
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func timeString(t time.Time) string {
	if t.IsZero() {
		return "(unset)"
	}
	return t.UTC().Format(time.RFC3339)
}

func (d *ServicePodDiagnostics) condition(typ string) (PodConditionSnapshot, bool) {
	for _, c := range d.Conditions {
		if c.Type == typ {
			return c, true
		}
	}
	return PodConditionSnapshot{}, false
}
