package engine

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// serviceContainerName is the single container buildServicePod puts in
// a service pod. Liveness is judged on THIS container only: a future
// sidecar exiting on its own must not condemn a service that is still
// answering.
const serviceContainerName = "service"

// servicePodDeath reports whether a service pod has stopped serving,
// and why. Empty string means "still serving, or not up yet".
//
// It exists because `status.podIP` is NOT a liveness signal. Kubernetes
// keeps the field populated on a pod in a terminal phase until the
// object is deleted, so a service that served for ten minutes and then
// died still presents a perfectly good-looking IP. Treating that IP as
// proof of life made EnsureServices adopt a dead run-scoped service:
// the agent emitted `ready`, wired the stale IP into the consumer job's
// hostAliases, and the job failed minutes later with `connection
// refused` in whatever code first touched the service — the same
// user-visible shape as #305, where the control plane reports a healthy
// service and the job looks broken.
//
// What counts as death, deliberately narrow:
//
//   - a terminal pod phase (Failed / Succeeded), or
//   - a terminated "service" container, which can be observed a poll
//     BEFORE the phase catches up.
//
// What does NOT count:
//
//   - Waiting, including ImagePullBackOff / CrashLoopBackOff. The pod
//     may simply not be up yet, and the startup wait owns that timeout
//     with its own error. Condemning here would turn a slow image pull
//     into an immediate hard failure.
//   - A container that restarted and is running again. It IS serving;
//     refusing to adopt it would fail a job over a service that works.
//     "It restarted at some point" is a warning for teardown
//     telemetry, not grounds to stop a job.
func servicePodDeath(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}

	var parts []string
	switch pod.Status.Phase {
	case corev1.PodFailed, corev1.PodSucceeded:
		part := "pod phase " + string(pod.Status.Phase)
		if pod.Status.Reason != "" {
			// Pod-level reason carries the platform's verdict —
			// "Evicted" being the one worth naming.
			part += " (" + pod.Status.Reason + ")"
		}
		parts = append(parts, part)
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != serviceContainerName {
			continue
		}
		if t := cs.State.Terminated; t != nil {
			part := fmt.Sprintf("container terminated with exit %d", t.ExitCode)
			if t.Reason != "" {
				part += " (" + t.Reason + ")"
			}
			if t.Signal != 0 {
				part += fmt.Sprintf(", signal %d", t.Signal)
			}
			parts = append(parts, part)
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}
