package deploy

// Evaluate classifies one Application convergence snapshot against the revision
// the deploy is waiting for. It is PURE and per-snapshot: sync + health + the
// observed revision.
//
// It deliberately does NOT consult OperationPhase. `.status.operationState.phase`
// persists the LAST sync operation and can be stale or unrelated to this deploy —
// a Synced+Healthy app on the right revision can still carry an old Failed op, so
// keying failure off it would false-fail a healthy deploy. Correlating the
// operation with THIS deploy (post-Sync, matching revision/timestamp) needs the
// watch loop's timing context, so operation-based fast-fail lives THERE (it reads
// DeployState.OperationPhase), not here.
//
// Rules:
//   - Degraded health → Failed. Health is computed live from the resources, so a
//     Degraded app is unhealthy NOW (the loop debounces transient flaps).
//   - not (Synced AND Healthy) → Pending.
//   - expectedRev set AND ObservedRev != expectedRev → Pending. Fail-closed
//     against declaring success on a stale/other revision (a healthy app on the
//     OLD revision right after Sync, or observe-mode drift).
//   - otherwise → Succeeded.
//
// When expectedRev is "" (unpinned), a Synced+Healthy snapshot is trusted as-is;
// the caller must only evaluate snapshots taken AFTER its own Sync operation has
// been observed, so a stale pre-Sync snapshot isn't mistaken for success.
func Evaluate(s DeployState, expectedRev string) DeployOutcome {
	if s.Health == HealthDegraded {
		return OutcomeFailed
	}
	if s.Sync != SyncSynced || s.Health != HealthHealthy {
		return OutcomePending
	}
	if expectedRev != "" && s.ObservedRev != expectedRev {
		return OutcomePending
	}
	return OutcomeSucceeded
}
