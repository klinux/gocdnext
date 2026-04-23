package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// ApproveGate flips an awaiting approval row to 'queued' and
// stamps decided_by + decided_at + decision='approved'. The
// transition is atomic via a conditional UPDATE so two concurrent
// approvals converge safely: one wins, the other sees zero rows
// affected and surfaces ErrApprovalNotPending.
//
// After approval the scheduler picks the row up on its next tick
// (same queue as any freshly-materialised job). The caller is
// responsible for firing a run_queued NOTIFY so dispatch doesn't
// wait for the polling tick — ApproveGate returns the run id for
// exactly that purpose.
func (s *Store) ApproveGate(ctx context.Context, d ApprovalDecision) (runID uuid.UUID, err error) {
	return s.decideGate(ctx, d, "approved", "queued")
}

// RejectGate flips an awaiting approval row to 'failed' with
// decision='rejected'. Downstream jobs `needs:`ing this one will
// never run — the stage cascade treats the rejection the same
// way it treats a failed script, which is the right semantics
// (a rejected deploy shouldn't silently skip the smoke test that
// follows it).
func (s *Store) RejectGate(ctx context.Context, d ApprovalDecision) (runID uuid.UUID, err error) {
	return s.decideGate(ctx, d, "rejected", "failed")
}

func (s *Store) decideGate(ctx context.Context, d ApprovalDecision, decision, nextStatus string) (uuid.UUID, error) {
	if d.JobRunID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("store: approval decision: job run id required")
	}

	// Pre-check approvers: ErrApproverNotAllowed must surface
	// distinctly from "wrong status". Doing the check here (vs.
	// in SQL) keeps the error ergonomics clean — the single
	// UPDATE would otherwise conflate "not pending" and "not
	// allowed" into the same zero-rows-affected outcome.
	var (
		gate      bool
		status    string
		approvers []string
		parentRun uuid.UUID
	)
	err := s.pool.QueryRow(ctx, `
		SELECT approval_gate, status, approvers, run_id
		FROM job_runs
		WHERE id = $1
	`, d.JobRunID).Scan(&gate, &status, &approvers, &parentRun)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrApprovalGateNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: load approval row: %w", err)
	}
	if !gate {
		return uuid.Nil, ErrApprovalGateNotFound
	}
	if status != "awaiting_approval" {
		return uuid.Nil, ErrApprovalNotPending
	}
	// Empty approvers list = "any authenticated user" (same
	// parse-time decision: permissive default, RBAC layers on
	// top). A non-empty list enforces membership.
	if len(approvers) > 0 && d.User != "" && !slices.Contains(approvers, d.User) {
		return uuid.Nil, ErrApproverNotAllowed
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE job_runs
		SET status      = $2,
		    decision    = $3,
		    decided_by  = $4,
		    decided_at  = NOW(),
		    finished_at = CASE WHEN $2 = 'failed' THEN NOW() ELSE finished_at END
		WHERE id = $1 AND status = 'awaiting_approval'
	`, d.JobRunID, nextStatus, decision, d.User)
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: apply approval decision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Concurrent decider won the race.
		return uuid.Nil, ErrApprovalNotPending
	}
	return parentRun, nil
}
