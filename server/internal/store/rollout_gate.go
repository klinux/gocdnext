package store

// Rollout approval-gate mutations (ADR-0001 Phase 2). The watcher is the only caller;
// every write is fenced on the watch's claim_id so a reclaimed (stale) watcher can't
// arm/action/clear a gate another replica now owns. The human decision path
// (DecideRolloutGate) and the cancel/supersede path land in sibling files.

import (
	"context"
	"errors"
	"fmt"

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
