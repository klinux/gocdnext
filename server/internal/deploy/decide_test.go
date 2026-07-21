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

// --- Phase 2 gate-driven control ---

func TestDecideRolloutGate(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	tp := func(d time.Duration) *time.Time { at := now.Add(d); return &at }
	ri := func(i int) *int { return &i }
	const window = 5 * time.Minute

	// App is Synced+Healthy+rev (Evaluate would succeed) — so every gate case here
	// exercises the gate precedence sitting ABOVE the classic success finalize.
	appOK := func(r RolloutState, observed bool) DeployState {
		return DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "rev1", Rollout: r, RolloutObserved: observed}
	}
	// Rollout snapshots. Observe always resolves the full identity (cluster/ns/name), so
	// the fixtures carry all three — the abort target is actionable.
	id := func(r RolloutState) RolloutState {
		r.ResolvedCluster, r.ResolvedNamespace, r.ResolvedName = "dest", "ns", "ro"
		return r
	}
	pausedIndef := id(RolloutState{Phase: RolloutPaused, PauseReason: PauseReasonCanaryStep, CurrentStepIndex: 1, CurrentStepKnown: true, StepCount: 3, PausedIndefinitely: true, StableHash: "a", PodHash: "b"})
	pausedTimed := id(RolloutState{Phase: RolloutPaused, PauseReason: PauseReasonCanaryStep, CurrentStepIndex: 1, CurrentStepKnown: true, StepCount: 3, PausedIndefinitely: false})
	promoted := id(RolloutState{Phase: RolloutHealthy, CurrentStepIndex: 3, CurrentStepKnown: true, StepCount: 3, FullyPromoted: true, StableHash: "b", PodHash: "b"})
	progressing := id(RolloutState{Phase: RolloutProgressing, CurrentStepIndex: 2, CurrentStepKnown: true, StepCount: 3})
	aborted := id(RolloutState{Phase: RolloutDegraded, Aborted: true})

	// Base anchors: rollout-aware + gated, sync fired, deadline 1h out.
	base := WatchAnchors{
		SyncMode: SyncModeTrigger, ExpectedRevision: "rev1", SyncRequestedAt: tp(-10 * time.Minute),
		DeadlineAt: now.Add(time.Hour), RolloutAware: true, Gated: true,
	}
	armed := base // armed at step 1, undecided, pinned
	armed.GateArmedAt, armed.GatePausedStep, armed.HasPinnedRolloutTarget = tp(-5*time.Minute), ri(1), true

	errState := func(observed bool) DeployState {
		s := appOK(RolloutState{}, observed)
		s.RolloutError = "the rollout could not be read from the cluster"
		return s
	}

	tests := []struct {
		name    string
		state   DeployState
		anchors WatchAnchors
		want    Verdict
	}{
		// ---- no early finalize (AC) ----
		{"rollout observed, not FullyPromoted → continue (App healthy notwithstanding)",
			appOK(progressing, true), base, Verdict{Effect: Continue}},
		{"rollout FullyPromoted → success", appOK(promoted, true), base, Verdict{Effect: FinalizeSuccess}},
		{"observe-only fallback (rollout NOT observed) → App-health success",
			appOK(RolloutState{}, false), base, Verdict{Effect: FinalizeSuccess}},

		// ---- arm only on an indefinite pause ----
		{"indefinite pause, not armed → ArmGate", appOK(pausedIndef, true), base, Verdict{Effect: ArmGate}},
		{"timed/analysis pause → NOT armed → continue", appOK(pausedTimed, true), base, Verdict{Effect: Continue}},
		{"indefinite pause but not gated (observe-only) → NOT armed → continue",
			appOK(pausedIndef, true), rolloutAware(base), Verdict{Effect: Continue}},

		// ---- deadline suspended ONLY while awaiting the human ----
		{"armed & undecided & PAST deadline → suspended → continue (still at armed step)",
			appOK(pausedIndef, true), withDeadline(armed, now.Add(-time.Second)), Verdict{Effect: Continue}},
		{"decided (approved) & past deadline & Observe error → deadline RESUMES → fail",
			errState(false), withDeadline(approved(armed), now.Add(-time.Second)),
			Verdict{Effect: FinalizeFailed, Reason: ReasonDeadlineExceeded}},

		// ---- abort-safe / promote-unsafe asymmetry under an Observe error ----
		{"approve under Observe error → does NOT promote → continue",
			errState(false), approved(armed), Verdict{Effect: Continue}},
		{"reject under Observe error → STILL aborts (pinned) ",
			errState(false), rejected(armed), Verdict{Effect: Abort, Reason: ReasonRejected}},

		// ---- approve path ----
		{"approved, not actioned, healthy observe → Promote",
			appOK(pausedIndef, true), approved(armed), Verdict{Effect: Promote}},
		{"approved, actioned, still paused at armed step → continue (controller latency)",
			appOK(pausedIndef, true), actioned(approved(armed)), Verdict{Effect: Continue}},
		{"approved, actioned, left the step → ClearGate",
			appOK(progressing, true), actioned(approved(armed)), Verdict{Effect: ClearGate}},

		// ---- reject path ----
		{"rejected, not actioned, not yet aborted → Abort",
			appOK(pausedIndef, true), rejected(armed), Verdict{Effect: Abort, Reason: ReasonRejected}},
		{"rejected, not actioned, ALREADY aborted → finalize (skip redundant abort)",
			appOK(aborted, true), rejected(armed), Verdict{Effect: FinalizeFailed, Reason: ReasonRejected}},
		{"rejected, actioned, aborted observed → FinalizeFailed(rejected)",
			appOK(aborted, true), actioned(rejected(armed)), Verdict{Effect: FinalizeFailed, Reason: ReasonRejected}},
		{"rejected, actioned, not yet aborted → continue (traffic-to-stable takes a beat)",
			appOK(progressing, true), actioned(rejected(armed)), Verdict{Effect: Continue}},

		// ---- external transitions ----
		{"external abort observed (no reject) → FinalizeFailed(rollout aborted)",
			appOK(aborted, true), base, Verdict{Effect: FinalizeFailed, Reason: ReasonRolloutAborted}},
		{"armed & undecided but externally promoted (progressing) → ClearGate",
			appOK(progressing, true), armed, Verdict{Effect: ClearGate}},
		{"armed at step 1 but now paused at step 3 (stale) → ClearGate",
			appOK(RolloutState{Phase: RolloutPaused, PauseReason: PauseReasonCanaryStep, CurrentStepIndex: 3, CurrentStepKnown: true, StepCount: 5, PausedIndefinitely: true, ResolvedName: "ro"}, true),
			armed, Verdict{Effect: ClearGate}},

		// ---- cancel / supersede (abort-safe, wins over all) ----
		{"cancel, not actioned, observed target → Abort(canceled)",
			appOK(pausedIndef, true), cancel(base), Verdict{Effect: Abort, Reason: ReasonCanceled}},
		{"cancel beats an approved gate → Abort(canceled), not Promote",
			appOK(pausedIndef, true), cancel(approved(armed)), Verdict{Effect: Abort, Reason: ReasonCanceled}},
		{"cancel, not actioned, NO known target (error, no pin) → continue (don't invent)",
			errState(false), cancel(base), Verdict{Effect: Continue}},
		{"cancel, actioned, aborted observed → FinalizeFailed(canceled)",
			appOK(aborted, true), abortActioned(cancel(base)), Verdict{Effect: FinalizeFailed, Reason: ReasonCanceled}},
		{"cancel, actioned, not yet aborted → continue",
			appOK(progressing, true), abortActioned(cancel(base)), Verdict{Effect: Continue}},

		// ---- defensive: a DECIDED gate must not actuate without an armed+pinned identity ----
		// (an impossible/corrupt state — persistence/migration/wiring bug). Fail closed:
		// never Promote/Abort an unknown target, never fall through to a success finalize.
		{"approved but NOT armed/pinned → continue (no Promote on an unknown target)",
			appOK(pausedIndef, true), approved(base), Verdict{Effect: Continue}},
		{"rejected but NOT armed/pinned → continue (no Abort on an unknown target)",
			appOK(pausedIndef, true), rejected(base), Verdict{Effect: Continue}},
		{"rejected but NOT armed/pinned, past deadline → fail closed (not success)",
			appOK(promoted, true), withDeadline(rejected(base), now.Add(-time.Second)),
			Verdict{Effect: FinalizeFailed, Reason: ReasonRejected}},
		{"cancel with an INCOMPLETE observed identity (no namespace) → continue (not a target)",
			DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "rev1", RolloutObserved: true,
				Rollout: RolloutState{Phase: RolloutPaused, ResolvedCluster: "c", ResolvedNamespace: "", ResolvedName: "ro"}},
			cancel(base), Verdict{Effect: Continue}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Decide(tt.state, tt.anchors, now, window); got != tt.want {
				t.Errorf("Decide = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func approved(w WatchAnchors) WatchAnchors     { w.GateDecision = GateApproved; return w }
func rejected(w WatchAnchors) WatchAnchors     { w.GateDecision = GateRejected; return w }
func actioned(w WatchAnchors) WatchAnchors     { at := w.DeadlineAt; w.GateActionedAt = &at; return w }
func rolloutAware(w WatchAnchors) WatchAnchors { w.RolloutAware = true; w.Gated = false; return w }
func cancel(w WatchAnchors) WatchAnchors       { w.CancelRequested = true; return w }
func abortActioned(w WatchAnchors) WatchAnchors {
	at := w.DeadlineAt
	w.RolloutAbortActionedAt = &at
	return w
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
