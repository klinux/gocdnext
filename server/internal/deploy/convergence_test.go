package deploy

import "testing"

// Evaluate is the heart of the watch loop: given one Application snapshot, decide
// whether the deploy converged (success), broke (failed), or is still in flight.
// Every ArgoCD sync/health combination that matters routes through here, so the
// table pins the corner cases the loop depends on.
func TestEvaluate(t *testing.T) {
	tests := []struct {
		name  string
		state DeployState
		want  DeployOutcome
	}{
		// The only success: the live state matches the desired revision AND the
		// resources are healthy.
		{"synced + healthy", DeployState{Sync: SyncSynced, Health: HealthHealthy}, OutcomeSucceeded},

		// Synced but still rolling — not done.
		{"synced + progressing", DeployState{Sync: SyncSynced, Health: HealthProgressing}, OutcomePending},

		// Healthy but not yet synced (a manual-sync app pre-sync, or drift) — not done.
		{"outofsync + healthy", DeployState{Sync: SyncOutOfSync, Health: HealthHealthy}, OutcomePending},

		// Degraded is a hard failure regardless of sync — the desired state is
		// unhealthy in the cluster. (The loop debounces transient flaps; the pure
		// per-snapshot classification is honest.)
		{"synced + degraded", DeployState{Sync: SyncSynced, Health: HealthDegraded}, OutcomeFailed},
		{"outofsync + degraded", DeployState{Sync: SyncOutOfSync, Health: HealthDegraded}, OutcomeFailed},

		// In-flight / transient states — keep watching (the loop's timeout is the
		// backstop for a stuck deploy).
		{"synced + missing", DeployState{Sync: SyncSynced, Health: HealthMissing}, OutcomePending},
		{"synced + suspended", DeployState{Sync: SyncSynced, Health: HealthSuspended}, OutcomePending},
		{"unknown + unknown", DeployState{Sync: SyncUnknown, Health: HealthUnknown}, OutcomePending},
		{"progressing health, unknown sync", DeployState{Sync: SyncUnknown, Health: HealthProgressing}, OutcomePending},

		// Empty/zero snapshot (first poll before any status) — pending, never a
		// false success.
		{"zero snapshot", DeployState{}, OutcomePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Evaluate(tt.state); got != tt.want {
				t.Errorf("Evaluate(%+v) = %q, want %q", tt.state, got, tt.want)
			}
		})
	}
}
