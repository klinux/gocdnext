package deploy

import "time"

// VerdictEffect is the small, idempotent action the watch loop should take for one
// observed snapshot. The loop applies it against the store (all the store mutations
// are idempotent/fenced). The Phase-2 gate effects (ArmGate/Promote/Abort/ClearGate)
// join the original convergence effects.
type VerdictEffect string

const (
	// Continue: keep watching, no state change this tick.
	Continue VerdictEffect = "continue"
	// SetDegraded: open the debounce window (first Degraded tick).
	SetDegraded VerdictEffect = "set_degraded"
	// ClearDegraded: health recovered before the window elapsed.
	ClearDegraded VerdictEffect = "clear_degraded"
	// FinalizeSuccess: the deploy converged — terminalize the revision success.
	FinalizeSuccess VerdictEffect = "finalize_success"
	// FinalizeFailed: terminalize the revision failed (Reason says why).
	FinalizeFailed VerdictEffect = "finalize_failed"

	// ArmGate: an indefinite canary pause reached — arm an approval gate on the watch.
	ArmGate VerdictEffect = "arm_gate"
	// Promote: an approved gate — advance the canary one step (clear pauseConditions).
	Promote VerdictEffect = "promote"
	// Abort: a rejected gate, or a cancel/supersede — revert traffic to stable.
	Abort VerdictEffect = "abort"
	// ClearGate: the rollout left the armed step (promoted/aborted, ours or external)
	// — disarm this step's gate; a later pause re-arms a fresh one.
	ClearGate VerdictEffect = "clear_gate"
)

// GateDecision is the human verdict recorded on the watch (empty = still awaiting).
type GateDecision string

const (
	GateUndecided GateDecision = ""
	GateApproved  GateDecision = "approved"
	GateRejected  GateDecision = "rejected"
)

// Reason strings for the fixed failure verdicts (op-failure reasons are built from
// the phase). Exported so the loop and tests share them verbatim.
const (
	ReasonDeadlineExceeded = "progress deadline exceeded"
	ReasonDegradedTooLong  = "degraded beyond debounce window"
	ReasonRolloutAborted   = "rollout aborted"
	ReasonRejected         = "deploy rejected at the approval gate"
	ReasonCanceled         = "deploy canceled"
)

// Verdict is Decide's output: the effect plus a human reason for FinalizeFailed.
type Verdict struct {
	Effect VerdictEffect
	Reason string
}

// WatchAnchors is the per-watch state Decide reads — a pure view of a
// store.DeployWatch, no DB types. The Phase-2 gate fields are all zero for a plain
// (non-rollout) deploy, in which case Decide behaves exactly as it did in PR1.
type WatchAnchors struct {
	SyncMode         SyncMode
	ExpectedRevision string
	SyncRequestedAt  *time.Time
	DeadlineAt       time.Time
	DegradedSince    *time.Time

	// Rollout gate control (Phase 2).
	RolloutAware bool // the target observes a Rollout
	Gated        bool // the target has a governing_gate (control mode)
	// CancelRequested mirrors cancel_requested_at on the deploy's job_run (supersede /
	// manual cancel). Wired in the cancel/supersede chunk; zero here means "not
	// canceled" so a plain deploy is unaffected.
	CancelRequested bool
	// GateArmedAt/GateDecision/GateActionedAt/GatePausedStep are the armed step's state.
	// GateArmedAt != nil && GateDecision == "" ⟺ awaiting the human (deadline suspended).
	GateArmedAt    *time.Time
	GateDecision   GateDecision
	GateActionedAt *time.Time // Promote/Abort issued for THIS gated decision
	GatePausedStep *int       // the step index the gate armed at (detects "left the step")
	// HasPinnedRolloutTarget is true once ArmGate pinned gate_rollout_name — the abort
	// target is known even under an Observe error. Non-gated cancels rely on the
	// this-tick observed identity instead.
	HasPinnedRolloutTarget bool
	// RolloutAbortActionedAt is the gate-independent anti-re-abort guard for the
	// cancel/supersede path (a non-gated rollout can be aborted too).
	RolloutAbortActionedAt *time.Time
}

// operationTrusted reports whether `.status.operationState` can be attributed to
// THIS deploy (and thus used for a correlated fast-fail). It requires a Sync anchor,
// a pinned revision, an operation that started at/after our Sync, and that synced our
// exact revision. A zero/absent OperationStartedAt fails closed.
func operationTrusted(state DeployState, w WatchAnchors) bool {
	if w.SyncRequestedAt == nil || w.ExpectedRevision == "" {
		return false
	}
	if state.OperationStartedAt.Before(*w.SyncRequestedAt) {
		return false
	}
	return state.SyncResultRevision == w.ExpectedRevision
}

// rolloutObservedAborted is the "the abort landed / an external abort happened" signal.
func rolloutObservedAborted(state DeployState) bool {
	return state.RolloutObserved && state.Rollout.Aborted
}

// pausedAtArmedStep reports whether the Rollout is still paused at the exact step the
// gate armed at. Used to tell "controller hasn't acted yet" (wait) from "left the step"
// (clear/re-arm). An unknown controller step index is NOT the armed step (fail-closed
// toward clearing, which re-arms on the next confirmed pause).
func pausedAtArmedStep(state DeployState, w WatchAnchors) bool {
	return state.RolloutObserved && state.Rollout.Phase == RolloutPaused &&
		w.GatePausedStep != nil && state.Rollout.CurrentStepKnown &&
		state.Rollout.CurrentStepIndex == *w.GatePausedStep
}

// hasKnownAbortTarget reports whether the watcher can name a Rollout to abort — the
// gate pin (armed) or the identity Observe resolved this tick. Under an Observe error
// with no pin, there is no target: don't invent one (stay pending).
func hasKnownAbortTarget(state DeployState, w WatchAnchors) bool {
	return w.HasPinnedRolloutTarget || (state.RolloutObserved && state.Rollout.ResolvedName != "")
}

// Decide is the pure brain of the watch loop. Given one convergence snapshot, the
// watch's anchors, the current time, and the Degraded debounce window, it returns the
// single effect to apply. Precedence, top wins (Phase-2 gate control sits above the
// classic convergence logic):
//
//	(1) cancel/supersede of a rollout deploy — abort-safe, wins over all.
//	(2) gate REJECT — abort-safe: fires even under a control-mode Observe error.
//	(3) rollout observed aborted (external / ours landed) → fail.
//	(4) deadline — SUSPENDED while awaiting the human (armed & undecided); the decision
//	    (not the action) resumes it, so a decision + failing Observe still deadline-fails.
//	(5) control-mode Observe error → pending; never App-health finalize. Promote (below)
//	    is unreachable here — abort is safe under uncertainty, promote is not.
//	(6) gate APPROVE → Promote (once); then wait for the controller, then ClearGate.
//	(7) indefinite pause + not armed → ArmGate (ONLY a `pause:{}` step).
//	(8) armed & undecided but no longer at the armed step (external promote) → ClearGate.
//	(9) classic convergence: success (gated by FullyPromoted when rollout-aware — no
//	    early finalize), correlated fast-fail, Degraded debounce.
func Decide(state DeployState, w WatchAnchors, now time.Time, degradedWindow time.Duration) Verdict {
	pastDeadline := now.After(w.DeadlineAt)

	// (1) Cancel/supersede of a ROLLOUT deploy. Abort-safe → above the Observe-error
	// gate. Non-rollout cancels are terminated outside the watcher, so this is
	// RolloutAware-only and a plain deploy is untouched.
	if w.CancelRequested && w.RolloutAware {
		if w.RolloutAbortActionedAt == nil {
			if hasKnownAbortTarget(state, w) {
				return Verdict{Effect: Abort, Reason: ReasonCanceled}
			}
			// No known target (Observe errored, no pin): can't abort — wait for Observe
			// to recover or the deadline. Never invent a target.
			if pastDeadline {
				return Verdict{Effect: FinalizeFailed, Reason: ReasonDeadlineExceeded}
			}
			return Verdict{Effect: Continue}
		}
		if rolloutObservedAborted(state) || pastDeadline {
			return Verdict{Effect: FinalizeFailed, Reason: ReasonCanceled}
		}
		return Verdict{Effect: Continue} // abort issued; wait to observe it / deadline
	}

	// (2) Gate REJECT — abort-safe. Fires even under a control-mode Observe error, via
	// the pinned identity. Skip a redundant Abort if we already observe it aborted.
	if w.Gated && w.GateDecision == GateRejected {
		if w.GateActionedAt == nil && !rolloutObservedAborted(state) {
			return Verdict{Effect: Abort, Reason: ReasonRejected}
		}
		if rolloutObservedAborted(state) || pastDeadline {
			return Verdict{Effect: FinalizeFailed, Reason: ReasonRejected}
		}
		return Verdict{Effect: Continue}
	}

	// (3) Rollout observed aborted outside a reject (external abort) → fail.
	if w.RolloutAware && rolloutObservedAborted(state) {
		return Verdict{Effect: FinalizeFailed, Reason: ReasonRolloutAborted}
	}

	// (4) Deadline — suspended only while awaiting the human.
	awaitingHuman := w.GateArmedAt != nil && w.GateDecision == GateUndecided
	if pastDeadline && !awaitingHuman {
		return Verdict{Effect: FinalizeFailed, Reason: ReasonDeadlineExceeded}
	}

	// (5) Control-mode Observe error → pending (fail-closed). An approved gate under an
	// Observe error lands HERE and does NOT promote (promote-unsafe under uncertainty).
	if w.Gated && w.RolloutAware && state.RolloutError != "" {
		return Verdict{Effect: Continue}
	}

	// (6) Gate APPROVE — only reachable on a healthy observe (past (5)).
	if w.Gated && w.GateDecision == GateApproved {
		if w.GateActionedAt == nil {
			return Verdict{Effect: Promote}
		}
		if pausedAtArmedStep(state, w) {
			return Verdict{Effect: Continue} // promoted; controller hasn't advanced yet
		}
		return Verdict{Effect: ClearGate} // left the step — disarm; a later pause re-arms
	}

	// (7) Arm a gate at an indefinite pause (only a `pause:{}` step, only when gated).
	if w.Gated && w.RolloutAware && state.RolloutObserved && state.Rollout.PausedIndefinitely && w.GateArmedAt == nil {
		return Verdict{Effect: ArmGate}
	}

	// (8) Armed & undecided but no longer at the armed step (external promote/abort
	// while awaiting) → clear the stale gate; a confirmed pause re-arms next tick.
	if awaitingHuman && !pausedAtArmedStep(state, w) {
		return Verdict{Effect: ClearGate}
	}

	// (9) Classic convergence.
	if Evaluate(state, w.ExpectedRevision) == OutcomeSucceeded {
		// No early finalize: a rollout-aware, observed deploy is done only when the
		// canary is FullyPromoted — a Synced+Healthy App mid-canary is not. For a
		// non-rollout deploy, or one that fell back to App health (not observed), the
		// default FullyPromoted=false must NOT block finalize, so gate on those.
		if !w.RolloutAware || !state.RolloutObserved || state.Rollout.FullyPromoted {
			return Verdict{Effect: FinalizeSuccess}
		}
		// else: suppress the early success; deadline/degraded below still apply.
	}
	if operationTrusted(state, w) {
		switch state.OperationPhase {
		case OpFailed, OpError:
			return Verdict{Effect: FinalizeFailed, Reason: "sync operation " + string(state.OperationPhase)}
		}
	}
	if state.Health == HealthDegraded {
		if w.DegradedSince == nil {
			return Verdict{Effect: SetDegraded}
		}
		if now.Sub(*w.DegradedSince) >= degradedWindow {
			return Verdict{Effect: FinalizeFailed, Reason: ReasonDegradedTooLong}
		}
		return Verdict{Effect: Continue}
	}
	if w.DegradedSince != nil {
		return Verdict{Effect: ClearDegraded}
	}
	return Verdict{Effect: Continue}
}
