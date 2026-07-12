package deploy

import "testing"

// Evaluate is the heart of the watch loop: given one Application snapshot and the
// revision we're waiting for, decide whether the deploy converged (success), broke
// (failed), or is still in flight. Every combination the loop depends on — and,
// crucially, the "don't declare success on the wrong/old revision" guard — routes
// through here.
func TestEvaluate(t *testing.T) {
	const rev = "abc123"
	tests := []struct {
		name     string
		state    DeployState
		expected string // the revision we're waiting for ("" = unknown/not pinned)
		want     DeployOutcome
	}{
		// Success requires Synced + Healthy AND (when we pinned a revision) that the
		// observed revision matches it.
		{"synced+healthy, rev matches", DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: rev}, rev, OutcomeSucceeded},
		{"synced+healthy, no expected rev", DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: rev}, "", OutcomeSucceeded},

		// THE HIGH FIX: healthy on the OLD revision (stale post-Sync, or observe-mode
		// drift) must NOT be a false success — keep watching (the loop's timeout is
		// the fail-closed backstop for a genuine, persistent mismatch).
		{"synced+healthy, rev mismatch (stale)", DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "OLD"}, rev, OutcomePending},
		{"synced+healthy, observed rev empty (multi-source)", DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: ""}, rev, OutcomePending},

		// A still-running sync operation means a stale Synced+Healthy isn't done yet.
		{"synced+healthy but operation Running", DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: rev, OperationPhase: OpRunning}, rev, OutcomePending},
		{"synced+healthy but operation Terminating", DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: rev, OperationPhase: OpTerminating}, rev, OutcomePending},
		{"synced+healthy, operation Succeeded, rev matches", DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: rev, OperationPhase: OpSucceeded}, rev, OutcomeSucceeded},

		// Hard failures.
		{"degraded is a failure regardless of sync", DeployState{Sync: SyncSynced, Health: HealthDegraded, ObservedRev: rev}, rev, OutcomeFailed},
		{"sync operation Failed", DeployState{Sync: SyncOutOfSync, Health: HealthProgressing, OperationPhase: OpFailed}, rev, OutcomeFailed},
		{"sync operation Error", DeployState{Sync: SyncOutOfSync, Health: HealthProgressing, OperationPhase: OpError}, rev, OutcomeFailed},

		// In-flight / transient — keep watching.
		{"synced+progressing", DeployState{Sync: SyncSynced, Health: HealthProgressing, ObservedRev: rev}, rev, OutcomePending},
		{"outofsync+healthy", DeployState{Sync: SyncOutOfSync, Health: HealthHealthy, ObservedRev: rev}, rev, OutcomePending},
		{"synced+missing", DeployState{Sync: SyncSynced, Health: HealthMissing, ObservedRev: rev}, rev, OutcomePending},
		{"synced+suspended", DeployState{Sync: SyncSynced, Health: HealthSuspended, ObservedRev: rev}, rev, OutcomePending},
		{"unknown+unknown", DeployState{Sync: SyncUnknown, Health: HealthUnknown}, rev, OutcomePending},
		{"zero snapshot", DeployState{}, "", OutcomePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Evaluate(tt.state, tt.expected); got != tt.want {
				t.Errorf("Evaluate(%+v, %q) = %q, want %q", tt.state, tt.expected, got, tt.want)
			}
		})
	}
}
