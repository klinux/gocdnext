package deploy

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	// Fixed clock; anchors are expressed as offsets from it.
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	tp := func(d time.Duration) *time.Time { at := now.Add(d); return &at }
	const window = 5 * time.Minute

	// Base anchors: trigger mode, pinned rev, Sync fired 10m ago, deadline 1h out,
	// not degraded. Cases override what they exercise.
	base := WatchAnchors{
		SyncMode:         SyncModeTrigger,
		ExpectedRevision: "rev1",
		SyncRequestedAt:  tp(-10 * time.Minute),
		DeadlineAt:       now.Add(1 * time.Hour),
		DegradedSince:    nil,
	}
	// A snapshot whose operationState is trustworthy for `base` (started after Sync,
	// synced the expected revision).
	trustedOp := func(phase OpPhase, sync SyncStatus, health HealthStatus) DeployState {
		return DeployState{
			Sync: sync, Health: health, ObservedRev: "rev1",
			OperationPhase: phase, OperationStartedAt: now.Add(-9 * time.Minute), SyncResultRevision: "rev1",
		}
	}
	converged := DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "rev1"}

	tests := []struct {
		name    string
		state   DeployState
		anchors WatchAnchors
		want    Verdict
	}{
		// ---- deadline wins over everything ----
		{
			name:    "past deadline beats a would-be success",
			state:   converged,
			anchors: withDeadline(base, now.Add(-1*time.Second)),
			want:    Verdict{Effect: FinalizeFailed, Reason: ReasonDeadlineExceeded},
		},
		{
			name:    "past deadline beats degraded-within-window",
			state:   DeployState{Sync: SyncOutOfSync, Health: HealthDegraded},
			anchors: withDegraded(withDeadline(base, now.Add(-1*time.Second)), tp(-1*time.Minute)),
			want:    Verdict{Effect: FinalizeFailed, Reason: ReasonDeadlineExceeded},
		},
		{
			name:    "past deadline beats a trusted running op",
			state:   trustedOp(OpRunning, SyncOutOfSync, HealthProgressing),
			anchors: withDeadline(base, now.Add(-1*time.Second)),
			want:    Verdict{Effect: FinalizeFailed, Reason: ReasonDeadlineExceeded},
		},

		// ---- success (Evaluate is the base) ----
		{
			name:    "synced+healthy+rev match → success",
			state:   converged,
			anchors: base,
			want:    Verdict{Effect: FinalizeSuccess},
		},
		{
			name:    "unpinned rev, synced+healthy → success",
			state:   DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "whatever"},
			anchors: withRev(base, ""),
			want:    Verdict{Effect: FinalizeSuccess},
		},
		{
			// A converged healthy app succeeds even if an earlier CORRELATED op failed:
			// operationState is never required for success (success precedence pin).
			name:    "converged healthy beats a trusted failed op",
			state:   trustedOp(OpFailed, SyncSynced, HealthHealthy),
			anchors: base,
			want:    Verdict{Effect: FinalizeSuccess},
		},
		{
			// Healthy but on the OLD revision right after Sync → not success (fail-closed),
			// falls through to Continue (health not degraded).
			name:    "synced+healthy but rev mismatch → continue",
			state:   DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "rev0"},
			anchors: base,
			want:    Verdict{Effect: Continue},
		},

		// ---- correlated fast-fail ----
		{
			name:    "trusted op Failed → fast-fail",
			state:   trustedOp(OpFailed, SyncOutOfSync, HealthProgressing),
			anchors: base,
			want:    Verdict{Effect: FinalizeFailed, Reason: "sync operation Failed"},
		},
		{
			name:    "trusted op Error → fast-fail",
			state:   trustedOp(OpError, SyncOutOfSync, HealthProgressing),
			anchors: base,
			want:    Verdict{Effect: FinalizeFailed, Reason: "sync operation Error"},
		},
		{
			// Correlated failure beats the debounce (step 3 before step 4).
			name:    "trusted op Failed beats degraded debounce",
			state:   trustedOp(OpFailed, SyncOutOfSync, HealthDegraded),
			anchors: base,
			want:    Verdict{Effect: FinalizeFailed, Reason: "sync operation Failed"},
		},
		{
			name:    "op Failed but started BEFORE sync → not trusted → continue",
			state:   DeployState{Sync: SyncOutOfSync, Health: HealthProgressing, OperationPhase: OpFailed, OperationStartedAt: now.Add(-11 * time.Minute), SyncResultRevision: "rev1"},
			anchors: base,
			want:    Verdict{Effect: Continue},
		},
		{
			name:    "op Failed but synced a DIFFERENT rev → not trusted → continue",
			state:   DeployState{Sync: SyncOutOfSync, Health: HealthProgressing, OperationPhase: OpFailed, OperationStartedAt: now.Add(-9 * time.Minute), SyncResultRevision: "other"},
			anchors: base,
			want:    Verdict{Effect: Continue},
		},
		{
			// Unpinned rev: never trust syncResult for fast-fail (reviewer's explicit rule).
			name:    "op Failed but rev unpinned → not trusted → continue",
			state:   DeployState{Sync: SyncOutOfSync, Health: HealthProgressing, OperationPhase: OpFailed, OperationStartedAt: now.Add(-9 * time.Minute), SyncResultRevision: ""},
			anchors: withRev(base, ""),
			want:    Verdict{Effect: Continue},
		},
		{
			name:    "trusted op Terminating is not a failure phase → continue",
			state:   trustedOp(OpTerminating, SyncOutOfSync, HealthProgressing),
			anchors: base,
			want:    Verdict{Effect: Continue},
		},

		// ---- degraded debounce ----
		{
			name:    "degraded, no anchor yet → set degraded",
			state:   DeployState{Sync: SyncOutOfSync, Health: HealthDegraded},
			anchors: base,
			want:    Verdict{Effect: SetDegraded},
		},
		{
			name:    "degraded, within window → continue",
			state:   DeployState{Sync: SyncOutOfSync, Health: HealthDegraded},
			anchors: withDegraded(base, tp(-2*time.Minute)),
			want:    Verdict{Effect: Continue},
		},
		{
			name:    "degraded, past window → fail",
			state:   DeployState{Sync: SyncOutOfSync, Health: HealthDegraded},
			anchors: withDegraded(base, tp(-6*time.Minute)),
			want:    Verdict{Effect: FinalizeFailed, Reason: ReasonDegradedTooLong},
		},
		{
			name:    "recovered from degraded → clear",
			state:   DeployState{Sync: SyncOutOfSync, Health: HealthProgressing},
			anchors: withDegraded(base, tp(-2*time.Minute)),
			want:    Verdict{Effect: ClearDegraded},
		},
		{
			name:    "healthy-ish progressing, never degraded → continue",
			state:   DeployState{Sync: SyncOutOfSync, Health: HealthProgressing},
			anchors: base,
			want:    Verdict{Effect: Continue},
		},

		// ---- observe mode (no Sync anchor) ----
		{
			name:    "observe: synced+healthy+rev → success without a sync anchor",
			state:   converged,
			anchors: observe(base),
			want:    Verdict{Effect: FinalizeSuccess},
		},
		{
			name:    "observe: degraded → debounce still applies",
			state:   DeployState{Sync: SyncOutOfSync, Health: HealthDegraded},
			anchors: observe(base),
			want:    Verdict{Effect: SetDegraded},
		},
		{
			name:    "observe: op Failed → not trusted (no anchor) → continue",
			state:   DeployState{Sync: SyncOutOfSync, Health: HealthProgressing, OperationPhase: OpFailed, OperationStartedAt: now.Add(-9 * time.Minute), SyncResultRevision: "rev1"},
			anchors: observe(base),
			want:    Verdict{Effect: Continue},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.state, tt.anchors, now, window)
			if got != tt.want {
				t.Errorf("Decide = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func withDeadline(w WatchAnchors, d time.Time) WatchAnchors { w.DeadlineAt = d; return w }
func withDegraded(w WatchAnchors, since *time.Time) WatchAnchors {
	w.DegradedSince = since
	return w
}
func withRev(w WatchAnchors, rev string) WatchAnchors { w.ExpectedRevision = rev; return w }
func observe(w WatchAnchors) WatchAnchors {
	w.SyncMode = SyncModeObserve
	w.SyncRequestedAt = nil
	return w
}
