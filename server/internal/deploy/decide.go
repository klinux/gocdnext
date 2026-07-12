package deploy

import "time"

// VerdictEffect is the small, idempotent action the watch loop should take for one
// observed snapshot. The loop applies it against the store (all the store mutations
// are idempotent: COALESCE degraded, fenced finalize).
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
)

// Reason strings for the fixed failure verdicts (op-failure reasons are built from
// the phase). Exported so the loop and tests share them verbatim.
const (
	ReasonDeadlineExceeded = "progress deadline exceeded"
	ReasonDegradedTooLong  = "degraded beyond debounce window"
)

// Verdict is Decide's output: the effect plus a human reason for FinalizeFailed.
type Verdict struct {
	Effect VerdictEffect
	Reason string
}

// WatchAnchors is the per-watch state Decide reads — a pure view of a
// store.DeployWatch, no DB types. Pointer fields are nil when unset:
//   - SyncRequestedAt: nil until this deploy's Sync fired (trigger mode); for
//     observe mode it stays nil (no sync is issued — existing state is observed).
//   - DegradedSince: nil while healthy; the open debounce anchor otherwise.
type WatchAnchors struct {
	SyncMode         SyncMode
	ExpectedRevision string
	SyncRequestedAt  *time.Time
	DeadlineAt       time.Time
	DegradedSince    *time.Time
}

// operationTrusted reports whether `.status.operationState` can be attributed to
// THIS deploy (and thus used for a correlated fast-fail). It requires:
//   - a Sync anchor (SyncRequestedAt != nil) — no anchor means observe mode or a
//     pre-Sync snapshot, where a persisted operationState is someone else's;
//   - a pinned revision (ExpectedRevision != "") — unpinned, we can't confirm the
//     operation targeted our revision, so we fail closed and let timeout/degraded
//     decide rather than trusting syncResult.revision blindly;
//   - the operation started at/after our Sync (OperationStartedAt >= SyncRequestedAt);
//     a zero/absent OperationStartedAt is before any real anchor → not trusted;
//   - the operation synced our exact revision (SyncResultRevision == ExpectedRevision).
func operationTrusted(state DeployState, w WatchAnchors) bool {
	if w.SyncRequestedAt == nil || w.ExpectedRevision == "" {
		return false
	}
	if state.OperationStartedAt.Before(*w.SyncRequestedAt) {
		return false
	}
	return state.SyncResultRevision == w.ExpectedRevision
}

// Decide is the pure brain of the watch loop: given one convergence snapshot, the
// watch's anchors, the current time, and the Degraded debounce window, it returns
// the single effect to apply. Precedence (each step wins over the ones below it):
//
//  1. Deadline wins over everything — past DeadlineAt the deploy failed its progress
//     budget, even if the snapshot looks promising (mirrors k8s progressDeadline).
//  2. Success is Evaluate's job (Synced + Healthy + expectedRev match); operationState
//     is never required to succeed, so a converged healthy app succeeds even if an
//     earlier correlated operation had failed.
//  3. Correlated fast-fail: a trusted operationState in a failure phase ends the
//     watch early instead of burning the whole deadline.
//  4. Degraded debounce: open the window, wait it out, then fail — or clear it on
//     recovery. A transient mid-rollout Degraded flap must not fail the deploy.
func Decide(state DeployState, w WatchAnchors, now time.Time, degradedWindow time.Duration) Verdict {
	if now.After(w.DeadlineAt) {
		return Verdict{Effect: FinalizeFailed, Reason: ReasonDeadlineExceeded}
	}
	if Evaluate(state, w.ExpectedRevision) == OutcomeSucceeded {
		return Verdict{Effect: FinalizeSuccess}
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
