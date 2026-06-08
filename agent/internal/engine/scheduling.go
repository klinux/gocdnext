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
// Each Toleration is DEEP-COPIED via cloneToleration: a naive
// `append(out, in...)` would copy the struct but alias the
// `*TolerationSeconds` pointer, so a later mutation on either side
// would back-propagate. cloneToleration copies the int64 into a
// fresh pointer so the result is truly independent of both inputs.
func concatTolerations(agent, profile []corev1.Toleration) []corev1.Toleration {
	if len(agent) == 0 && len(profile) == 0 {
		return nil
	}
	out := make([]corev1.Toleration, 0, len(agent)+len(profile))
	for _, t := range agent {
		out = append(out, cloneToleration(t))
	}
	for _, t := range profile {
		out = append(out, cloneToleration(t))
	}
	return out
}

// cloneToleration returns a fully-independent copy of t. The struct
// fields are scalars (strings + typed enums) which the struct
// assignment already copies, but TolerationSeconds is a `*int64`
// shared by default. Allocate a fresh int64 + new pointer when
// non-nil so mutating either side after the clone can't leak.
func cloneToleration(t corev1.Toleration) corev1.Toleration {
	out := t
	if t.TolerationSeconds != nil {
		v := *t.TolerationSeconds
		out.TolerationSeconds = &v
	}
	return out
}
