package engine

import (
	corev1 "k8s.io/api/core/v1"
)

// mergeNodeSelector combines the agent-level NodeSelector with a
// per-spec NodeSelector. Profile values WIN on key collisions —
// profile is more specific than the agent's StatefulSet default.
// Returns nil when both are empty so the pod spec stays minimal.
//
// Inputs are not mutated; the result is a fresh map even when only
// one side contributes, so a caller mutating the returned value
// can't accidentally rewrite the agent's NodeSelector for every
// future job on the same agent.
func mergeNodeSelector(agent, profile map[string]string) map[string]string {
	if len(agent) == 0 && len(profile) == 0 {
		return nil
	}
	out := make(map[string]string, len(agent)+len(profile))
	for k, v := range agent {
		out[k] = v
	}
	for k, v := range profile {
		out[k] = v
	}
	return out
}

// concatTolerations appends per-spec Tolerations to the agent-level
// list. Kubelet ignores exact duplicates, so we don't bother deduping
// here — the cost of detecting equality on Toleration (5 fields,
// including a *int64) outweighs the benefit of saving the bytes.
// Returns nil when both inputs are empty.
//
// Inputs are not mutated; the result is a fresh slice independent of
// either input so future mutations don't leak across pods.
func concatTolerations(agent, profile []corev1.Toleration) []corev1.Toleration {
	if len(agent) == 0 && len(profile) == 0 {
		return nil
	}
	out := make([]corev1.Toleration, 0, len(agent)+len(profile))
	out = append(out, agent...)
	out = append(out, profile...)
	return out
}
