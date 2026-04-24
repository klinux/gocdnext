package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrApprovalGateNotFound signals no matching approval row exists
// (either the job_run id is unknown, or it isn't an approval gate
// at all). Callers surface this as 404.
var ErrApprovalGateNotFound = errors.New("store: approval gate not found")

// ErrApprovalNotPending signals the row exists but is past its
// decision window — already approved, already rejected, or was
// never in awaiting_approval to begin with. Callers map this to
// 409 Conflict so the UI can distinguish "bad id" from "decision
// came in twice".
var ErrApprovalNotPending = errors.New("store: approval gate is not pending")

// ErrApproverNotAllowed signals the user isn't in the gate's
// approvers allow-list. Callers map this to 403.
var ErrApproverNotAllowed = errors.New("store: user not in approvers list")

// ApprovalDecision bundles the per-call inputs for Approve/Reject.
// Keeping them in a struct lets the HTTP layer pass both in one
// store call without a multi-arg signature that rots when fields
// move (decided-by comes from session context, not the POST body).
type ApprovalDecision struct {
	JobRunID uuid.UUID
	User     string // empty = anonymous (dev/demo; prod enforces auth at HTTP)
}

// ApprovalResult is returned from Approve/RejectGate so the HTTP
// layer can decide whether to fire run_queued NOTIFY (only when
// the transition left the run in a state the scheduler should
// re-evaluate) and what status the stage/run eventually settled
// on.
type ApprovalResult struct {
	RunID          uuid.UUID
	StageCompleted bool
	StageStatus    string
	RunCompleted   bool
	RunStatus      string
}

// ApproveGate flips an awaiting approval row directly to 'success'
// and cascades into stage + run promotion in a single transaction.
// Skipping the intermediate 'queued' status closes a race where
// the scheduler could pick up a gate between the flip and the
// cascade and try to dispatch a job with no tasks. The transition
// is atomic via a conditional UPDATE so two concurrent approvals
// converge: one wins, the other sees zero rows affected and gets
// ErrApprovalNotPending.
//
// Returns the run id so the HTTP layer can fire run_queued NOTIFY
// when the stage completed (next stage may have waiting work).
func (s *Store) ApproveGate(ctx context.Context, d ApprovalDecision) (ApprovalResult, error) {
	return s.decideGate(ctx, d, "approved", "success")
}

// RejectGate flips an awaiting approval row to 'failed' with
// decision='rejected' and cascades the stage failure — which in
// turn cancels downstream queued work via the shared cascade
// helper. A rejected deploy won't leave "ready to approve" ghosts
// sitting in a stage that'll never run.
func (s *Store) RejectGate(ctx context.Context, d ApprovalDecision) (ApprovalResult, error) {
	return s.decideGate(ctx, d, "rejected", "failed")
}

func (s *Store) decideGate(ctx context.Context, d ApprovalDecision, decision, nextStatus string) (ApprovalResult, error) {
	if d.JobRunID == uuid.Nil {
		return ApprovalResult{}, fmt.Errorf("store: approval decision: job run id required")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ApprovalResult{}, fmt.Errorf("store: approval begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Load the row under the tx so the allow-list check, the
	// "is it pending?" check, and the UPDATE all see the same
	// snapshot. Checking approvers in Go (vs. in SQL) keeps the
	// error ergonomics distinct: "not allowed" vs. "not pending"
	// vs. "not found" each surface a dedicated sentinel.
	var (
		gate       bool
		status     string
		approvers  []string
		parentRun  pgtype.UUID
		stageRunID pgtype.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT approval_gate, status, approvers, run_id, stage_run_id
		FROM job_runs
		WHERE id = $1
	`, d.JobRunID).Scan(&gate, &status, &approvers, &parentRun, &stageRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApprovalResult{}, ErrApprovalGateNotFound
	}
	if err != nil {
		return ApprovalResult{}, fmt.Errorf("store: load approval row: %w", err)
	}
	if !gate {
		return ApprovalResult{}, ErrApprovalGateNotFound
	}
	if status != "awaiting_approval" {
		return ApprovalResult{}, ErrApprovalNotPending
	}
	// Empty approvers list = "any authenticated user" (same
	// parse-time decision: permissive default, RBAC layers on
	// top). A non-empty list enforces membership.
	if len(approvers) > 0 && d.User != "" && !slices.Contains(approvers, d.User) {
		return ApprovalResult{}, ErrApproverNotAllowed
	}

	tag, err := tx.Exec(ctx, `
		UPDATE job_runs
		SET status      = $2,
		    decision    = $3,
		    decided_by  = $4,
		    decided_at  = NOW(),
		    finished_at = NOW()
		WHERE id = $1 AND status = 'awaiting_approval'
	`, d.JobRunID, nextStatus, decision, d.User)
	if err != nil {
		return ApprovalResult{}, fmt.Errorf("store: apply approval decision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Concurrent decider won the race.
		return ApprovalResult{}, ErrApprovalNotPending
	}

	// Cascade: same helper CompleteJob uses so a gate's success
	// promotes its stage (and onto the run) exactly like a
	// regular job's success would, and a rejection fans out as
	// a stage failure that cancels downstream queued work.
	q := s.q.WithTx(tx)
	var comp JobCompletion
	if err := cascadeAfterJobCompletion(ctx, q, stageRunID, parentRun, &comp); err != nil {
		return ApprovalResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return ApprovalResult{}, fmt.Errorf("store: approval commit: %w", err)
	}

	return ApprovalResult{
		RunID:          fromPgUUID(parentRun),
		StageCompleted: comp.StageCompleted,
		StageStatus:    comp.StageStatus,
		RunCompleted:   comp.RunCompleted,
		RunStatus:      comp.RunStatus,
	}, nil
}
