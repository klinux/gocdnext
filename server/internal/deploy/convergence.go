package deploy

// Evaluate classifies one Application convergence snapshot. It is pure and
// per-snapshot; the watch loop layers timeout and flap-debounce on top.
//
// Rules:
//   - Degraded → Failed: the rendered desired state is unhealthy in the cluster,
//     regardless of sync status. (A brief Degraded during a normal roll is
//     debounced by the loop, not swallowed here.)
//   - Synced AND Healthy → Succeeded: the live state matches the desired revision
//     and the resources are healthy — the only success.
//   - everything else (Progressing, OutOfSync, Suspended, Missing, Unknown, or a
//     zero snapshot before the first status) → Pending: keep watching. The loop's
//     timeout turns a genuinely stuck deploy into a failure.
func Evaluate(s DeployState) DeployOutcome {
	if s.Health == HealthDegraded {
		return OutcomeFailed
	}
	if s.Sync == SyncSynced && s.Health == HealthHealthy {
		return OutcomeSucceeded
	}
	return OutcomePending
}
