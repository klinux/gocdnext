package deploy

// Evaluate classifies one Application convergence snapshot against the revision
// the deploy is waiting for. It is pure and per-snapshot; the watch loop layers
// timeout, flap-debounce, and — for trigger mode — "observe a POST-Sync operation
// before trusting a Succeeded phase" on top.
//
// expectedRev is the git revision the deploy should converge to, or "" when it
// isn't pinned (best-effort). Rules, in order:
//
//   - Degraded health, or a Failed/Error sync operation → Failed. The desired
//     state is unhealthy or the sync itself failed.
//   - Not (Synced AND Healthy) → Pending. The only success needs the live state
//     to match the desired state and be healthy.
//   - Operation still Running/Terminating → Pending. A sync is in flight, so a
//     currently-reported Synced+Healthy may predate it.
//   - expectedRev set AND ObservedRev != expectedRev → Pending. FAIL-CLOSED
//     against declaring success on a stale/other revision: a healthy app on the
//     OLD revision (right after Sync, before the controller reconciles) or
//     observe-mode drift is not "done". A persistent mismatch is caught by the
//     loop's timeout, not swallowed here.
//   - otherwise → Succeeded.
func Evaluate(s DeployState, expectedRev string) DeployOutcome {
	if s.Health == HealthDegraded || s.OperationPhase == OpFailed || s.OperationPhase == OpError {
		return OutcomeFailed
	}
	if s.Sync != SyncSynced || s.Health != HealthHealthy {
		return OutcomePending
	}
	if s.OperationPhase == OpRunning || s.OperationPhase == OpTerminating {
		return OutcomePending
	}
	if expectedRev != "" && s.ObservedRev != expectedRev {
		return OutcomePending
	}
	return OutcomeSucceeded
}
