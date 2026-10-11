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

// ContainerTermination is a flattened corev1.ContainerStateTerminated.
type ContainerTermination struct {
	Reason     string
	Message    string
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

	// State is "running", "waiting", "terminated" or "unknown", with
	// StateReason/StateMessage carrying kubelet's explanation for the
	// waiting and terminated cases (ImagePullBackOff, CrashLoopBackOff,
	// OOMKilled, …).
	State        string
	StateReason  string
	StateMessage string

	// LastTermination is non-nil only when the container died at least
	// once — i.e. exactly the "it went away and came back" case.
	LastTermination *ContainerTermination

	CPURequest string
	MemRequest string
	CPULimit   string
	MemLimit   string
}

// PodConditionSnapshot is a flattened corev1.PodCondition.
type PodConditionSnapshot struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

// ServicePodDiagnostics is the whole snapshot, taken at teardown.
type ServicePodDiagnostics struct {
	PodName  string
	Phase    string
	Reason   string // pod-level, e.g. "Evicted"
	Message  string
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
		Message:  pod.Status.Message,
		NodeName: pod.Spec.NodeName,
	}
	for _, c := range pod.Status.Conditions {
		d.Conditions = append(d.Conditions, PodConditionSnapshot{
			Type:    string(c.Type),
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
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
			cd.StateMessage = cs.State.Waiting.Message
		case cs.State.Terminated != nil:
			cd.State = "terminated"
			cd.StateReason = cs.State.Terminated.Reason
			cd.StateMessage = cs.State.Terminated.Message
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			cd.LastTermination = &ContainerTermination{
				Reason:     t.Reason,
				Message:    t.Message,
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
// Resource keys report "(none)" rather than "" when the field was never
// set: "no memory ceiling" is a finding, and an empty string reads as
// "we didn't look".
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
	if d.Message != "" {
		attrs = append(attrs, "svc_pod_message", d.Message)
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
		if c.StateMessage != "" {
			attrs = append(attrs, "svc_state_message"+suffix, c.StateMessage)
		}
		// Always emit the last-termination keys, even absent: a reader
		// grepping for svc_last_exit_code needs to distinguish "0, died
		// cleanly" from "never died".
		if t := c.LastTermination; t != nil {
			attrs = append(attrs,
				"svc_last_exit_code"+suffix, t.ExitCode,
				"svc_last_signal"+suffix, t.Signal,
				"svc_last_reason"+suffix, orNone(t.Reason),
				"svc_last_started_at"+suffix, timeString(t.StartedAt),
				"svc_last_finished_at"+suffix, timeString(t.FinishedAt),
			)
			if t.Message != "" {
				attrs = append(attrs, "svc_last_message"+suffix, t.Message)
			}
		} else {
			attrs = append(attrs,
				"svc_last_exit_code"+suffix, "(never terminated)",
				"svc_last_reason"+suffix, "(never terminated)",
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
