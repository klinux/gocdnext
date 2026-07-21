package store

// Rollout approval-gate mutations (ADR-0001 Phase 2). The watcher is the only caller;
// every write is fenced on the watch's claim_id so a reclaimed (stale) watcher can't
// arm/action/clear a gate another replica now owns. The human decision path
// (DecideRolloutGate) and the cancel/supersede path land in sibling files.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrIncompleteGatePin is returned when ArmRolloutGate is asked to arm without a
// complete, actionable Rollout identity. It's a contract violation (the watcher must
// resolve the full pin before arming), surfaced loudly rather than persisting a gate
// that could never be Promoted/Aborted.
var ErrIncompleteGatePin = errors.New("store: arm rollout gate requires a complete pinned identity (cluster/namespace/name) and a non-negative step")

// ArmRolloutGateInput is the resolved, COMPLETE Rollout identity + step the gate pins.
// The watcher only arms with all three identity fields set (Promote/Abort act on them,
// never a re-discovery), and the paused step (Decide keys "still at the armed step" on
// it to avoid an early clear/re-arm).
type ArmRolloutGateInput struct {
	PausedStep       int
	RolloutCluster   string
	RolloutNamespace string
	RolloutName      string
}

// ArmRolloutGate arms the approval gate for the current indefinite canary pause,
// stamping a fresh gate_id + the pinned identity + the paused step. Fenced on claim_id;
// `gate_id IS NULL` makes a re-arm a no-op. Returns whether it armed (false = lease lost
// or already armed).
func (s *Store) ArmRolloutGate(ctx context.Context, revID, claimID uuid.UUID, in ArmRolloutGateInput) (bool, error) {
	// A gate must be armed with an actionable pin — an empty cluster/namespace/name would
	// stamp gate_id yet leave nothing to Promote/Abort, and (gate_id now set) it could
	// never re-arm with a correct pin. Reject at the edge (the query trusts this).
	if in.RolloutCluster == "" || in.RolloutNamespace == "" || in.RolloutName == "" || in.PausedStep < 0 {
		return false, ErrIncompleteGatePin
	}
	step := int32(in.PausedStep)
	n, err := s.q.ArmRolloutGate(ctx, db.ArmRolloutGateParams{
		DeploymentRevisionID: pgUUID(revID),
		ClaimID:              pgUUID(claimID),
		GatePausedStep:       &step,
		GateRolloutCluster:   nullableString(in.RolloutCluster),
		GateRolloutNamespace: nullableString(in.RolloutNamespace),
		GateRolloutName:      nullableString(in.RolloutName),
	})
	if err != nil {
		return false, fmt.Errorf("store: arm rollout gate: %w", err)
	}
	return n > 0, nil
}

// MarkGateActioned records that the gated Promote/Abort was issued (anti-retry only —
// no deadline change; the terminal decision already resumed the deadline). Fenced on
// claim_id; idempotent. Returns whether it stamped (false = lease lost or already set).
func (s *Store) MarkGateActioned(ctx context.Context, revID, claimID uuid.UUID) (bool, error) {
	n, err := s.q.MarkGateActioned(ctx, db.MarkGateActionedParams{
		DeploymentRevisionID: pgUUID(revID),
		ClaimID:              pgUUID(claimID),
	})
	if err != nil {
		return false, fmt.Errorf("store: mark gate actioned: %w", err)
	}
	return n > 0, nil
}

// MarkRolloutAbortActioned records the cancel/supersede abort in one tx: stamp
// rollout_abort_actioned_at (the gate-independent anti-re-abort guard), DISARM any armed
// gate — decided or not, since a cancel outranks a reject — AND delete the step's votes.
// The deadline is resumed once only if the gate was still UNDECIDED (a decided gate
// already resumed it in DecideRolloutGate — no double-shift). Fenced on claim_id; the
// guard makes a re-tick a no-op. Returns whether it stamped (false = lease lost or already
// actioned).
func (s *Store) MarkRolloutAbortActioned(ctx context.Context, revID, claimID uuid.UUID) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("store: mark rollout abort begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	n, err := q.MarkRolloutAbortActioned(ctx, db.MarkRolloutAbortActionedParams{
		DeploymentRevisionID: pgUUID(revID),
		ClaimID:              pgUUID(claimID),
	})
	if err != nil {
		return false, fmt.Errorf("store: mark rollout abort actioned: %w", err)
	}
	if n == 0 {
		return false, nil // lease lost or already actioned — leave votes alone
	}
	if err := q.DeleteDeployGateVotes(ctx, pgUUID(revID)); err != nil {
		return false, fmt.Errorf("store: mark rollout abort votes: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: mark rollout abort commit: %w", err)
	}
	return true, nil
}

// DeployWatchCancelRequestedAt reports the deploy's cancel/supersede intent
// (job_runs.cancel_requested_at) for the watcher's cancel-abort path. Nil = not canceled
// (or no owning job / unknown revision — treated as not canceled, fail-safe: a genuine
// cancel is re-read next tick).
func (s *Store) DeployWatchCancelRequestedAt(ctx context.Context, revID uuid.UUID) (*time.Time, error) {
	ts, err := s.q.GetDeployWatchCancelRequestedAt(ctx, pgUUID(revID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: deploy watch cancel-requested: %w", err)
	}
	return pgTimePtr(ts), nil
}

// ClearRolloutGate disarms the current step's gate in one tx: null the per-arm/decision/
// action columns (resuming the deadline with the one-time shift IF it was still
// undecided — an external promote before any vote) AND delete the step's votes, so the
// next pause re-arms with a clean slate. The CONFIG columns (approvers/required/…)
// persist. Fenced on claim_id; the vote-delete runs only after the fenced clear
// succeeds. Returns whether it cleared (false = lease lost).
func (s *Store) ClearRolloutGate(ctx context.Context, revID, claimID uuid.UUID) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("store: clear rollout gate begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	n, err := q.ClearRolloutGateColumns(ctx, db.ClearRolloutGateColumnsParams{
		DeploymentRevisionID: pgUUID(revID),
		ClaimID:              pgUUID(claimID),
	})
	if err != nil {
		return false, fmt.Errorf("store: clear rollout gate: %w", err)
	}
	if n == 0 {
		return false, nil // lease lost — leave the votes untouched
	}
	if err := q.DeleteDeployGateVotes(ctx, pgUUID(revID)); err != nil {
		return false, fmt.Errorf("store: clear rollout gate votes: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: clear rollout gate commit: %w", err)
	}
	return true, nil
}

// rolloutTextMax bounds cluster-supplied message/error so a giant status can't bloat
// the DB/UI (runes, so truncation never splits a UTF-8 sequence).
const rolloutTextMax = 500

// RolloutObservationInput is the observed Rollout snapshot the watcher persists each
// tick. Observed=false means the Rollout couldn't be read/resolved this tick — only
// Error is meaningful then. StepKnown=false NULLs rollout_current_step (an absent
// controller index must not read as step 0).
type RolloutObservationInput struct {
	Observed    bool
	Phase       string
	Message     string
	PauseReason string
	CurrentStep int
	StepKnown   bool
	StepCount   int
	Aborted     bool
	Error       string
}

// StampRolloutObservation persists the snapshot onto the watch, fenced on claimID
// (false → lease lost). A good observe clears rollout_error; a failed one records the
// (already sanitized) Error and NULLs the rest.
func (s *Store) StampRolloutObservation(ctx context.Context, revID, claimID uuid.UUID, in RolloutObservationInput) (bool, error) {
	p := db.StampRolloutObservationParams{
		DeploymentRevisionID: pgUUID(revID),
		ClaimID:              pgUUID(claimID),
	}
	if in.Observed {
		p.RolloutPhase = nullableString(in.Phase)
		p.RolloutMessage = nullableString(truncateRunes(in.Message, rolloutTextMax))
		p.RolloutPauseReason = nullableString(in.PauseReason)
		if in.StepKnown {
			v := int32(in.CurrentStep)
			p.RolloutCurrentStep = &v
		}
		sc := int32(in.StepCount)
		p.RolloutStepCount = &sc
		ab := in.Aborted
		p.RolloutAborted = &ab
	} else {
		p.RolloutError = nullableString(truncateRunes(in.Error, rolloutTextMax))
	}
	n, err := s.q.StampRolloutObservation(ctx, p)
	if err != nil {
		return false, fmt.Errorf("store: stamp rollout observation: %w", err)
	}
	return n > 0, nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
