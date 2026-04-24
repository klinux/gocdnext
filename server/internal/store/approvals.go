package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

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
	// UserID is the authenticated user's uuid — the primary
	// identity for group-membership checks and the stable key
	// for job_run_approvals. uuid.Nil is only acceptable in dev/
	// demo modes with anonymous access, in which case User below
	// still records who (the prod HTTP path never passes nil).
	UserID uuid.UUID
	// User is the display label for audit trails — name preferred,
	// email as fallback. Matched against the gate's approvers
	// array (string-compare), distinct from UserID.
	User string
	// Comment is optional — "LGTM, merging after 2pm" etc. Shown
	// in the detail trail alongside the vote.
	Comment string
}

// ApprovalResult is returned from Approve/RejectGate so the HTTP
// layer can decide whether to fire run_queued NOTIFY (only when
// the transition left the run in a state the scheduler should
// re-evaluate) and what status the stage/run eventually settled
// on.
//
// PendingQuorum is true when this approval counted but more votes
// are still needed before the gate passes — the HTTP layer then
// returns 202 Accepted with "n of m" info instead of the 200 that
// signals a final transition.
type ApprovalResult struct {
	RunID          uuid.UUID
	StageCompleted bool
	StageStatus    string
	RunCompleted   bool
	RunStatus      string

	PendingQuorum      bool
	ApprovalsNow       int
	ApprovalsRequired  int
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

	// Load the gate row under the tx so the allow-list check, the
	// "is it pending?" check, and any UPDATE all see the same
	// snapshot. Checking allow-list in Go (vs. in SQL) keeps the
	// error ergonomics distinct: "not allowed" vs. "not pending"
	// vs. "not found" each surface a dedicated sentinel.
	var (
		gate           bool
		status         string
		approvers      []string
		approverGroups []string
		required       int32
		parentRun      pgtype.UUID
		stageRunID     pgtype.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT approval_gate, status, approvers, approver_groups, approval_required, run_id, stage_run_id
		FROM job_runs
		WHERE id = $1
	`, d.JobRunID).Scan(&gate, &status, &approvers, &approverGroups, &required, &parentRun, &stageRunID)
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
	if required < 1 {
		required = 1
	}

	// Allow-list check: the user is either in `approvers` by name
	// OR in one of the groups listed in `approver_groups`. Empty
	// BOTH lists = "any authenticated user" (permissive default,
	// same as the pre-groups era). Group intersection requires a
	// second read, skipped when the gate lists no groups.
	if len(approvers) > 0 || len(approverGroups) > 0 {
		allowed := d.User != "" && slices.Contains(approvers, d.User)
		if !allowed && len(approverGroups) > 0 && d.UserID != uuid.Nil {
			q := s.q.WithTx(tx)
			names, err := q.ListUserGroupNames(ctx, pgUUID(d.UserID))
			if err != nil {
				return ApprovalResult{}, fmt.Errorf("store: load user groups: %w", err)
			}
			for _, n := range names {
				if slices.Contains(approverGroups, n) {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			return ApprovalResult{}, ErrApproverNotAllowed
		}
	}

	// Quorum accounting only runs when the caller supplied a
	// UserID — that's the authenticated HTTP path. Anonymous
	// callers (dev/demo mode, legacy tests without auth) skip
	// the vote table and fall through to the single-flip path,
	// preserving pre-groups semantics bit-for-bit.
	if d.UserID != uuid.Nil {
		// Record the vote. Unique (job_run_id, user_id) prevents
		// a user from double-counting toward quorum. ON CONFLICT
		// DO NOTHING makes re-posts idempotent — we still evaluate
		// quorum after so a duplicate approve call converges on
		// the same outcome.
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_run_approvals
			    (job_run_id, user_id, user_label, decision, comment)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (job_run_id, user_id) DO NOTHING
		`, d.JobRunID, d.UserID, d.User, decision, d.Comment); err != nil {
			return ApprovalResult{}, fmt.Errorf("store: record vote: %w", err)
		}

		// Reject path: one rejection from an allowed user fails
		// the gate immediately. No quorum accumulation.
		//
		// Approve path: count approved votes. If >= required,
		// flip to success + cascade. Otherwise keep the gate
		// pending and return PendingQuorum=true.
		if decision == "approved" {
			var approvedCount int32
			if err := tx.QueryRow(ctx, `
				SELECT COUNT(*) FROM job_run_approvals
				WHERE job_run_id = $1 AND decision = 'approved'
			`, d.JobRunID).Scan(&approvedCount); err != nil {
				return ApprovalResult{}, fmt.Errorf("store: count approvals: %w", err)
			}
			if approvedCount < required {
				if err := tx.Commit(ctx); err != nil {
					return ApprovalResult{}, fmt.Errorf("store: approval commit: %w", err)
				}
				return ApprovalResult{
					RunID:             fromPgUUID(parentRun),
					PendingQuorum:     true,
					ApprovalsNow:      int(approvedCount),
					ApprovalsRequired: int(required),
				}, nil
			}
		}
	}

	// Terminal transition — approved quorum hit, or a rejection.
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
		// Concurrent decider won the race (their terminal UPDATE
		// landed first). Our vote is persisted but the gate is
		// already decided — report that.
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
		RunID:             fromPgUUID(parentRun),
		StageCompleted:    comp.StageCompleted,
		StageStatus:       comp.StageStatus,
		RunCompleted:      comp.RunCompleted,
		RunStatus:         comp.RunStatus,
		ApprovalsNow:      int(required), // quorum hit or reject (full if approved)
		ApprovalsRequired: int(required),
	}, nil
}

// JobRunApprovalVote is one entry in the detail trail returned
// to the UI alongside a gate — "alice approved 2m ago: LGTM".
type JobRunApprovalVote struct {
	UserID    uuid.UUID
	UserLabel string
	Decision  string
	Comment   string
	DecidedAt time.Time
}

// ListJobRunApprovals returns the per-vote trail for a gate,
// oldest first. Empty slice when no one has voted yet.
func (s *Store) ListJobRunApprovals(ctx context.Context, jobRunID uuid.UUID) ([]JobRunApprovalVote, error) {
	rows, err := s.q.ListJobRunApprovals(ctx, pgUUID(jobRunID))
	if err != nil {
		return nil, fmt.Errorf("store: list job_run approvals: %w", err)
	}
	out := make([]JobRunApprovalVote, 0, len(rows))
	for _, r := range rows {
		out = append(out, JobRunApprovalVote{
			UserID:    fromPgUUID(r.UserID),
			UserLabel: r.UserLabel,
			Decision:  r.Decision,
			Comment:   r.Comment,
			DecidedAt: r.DecidedAt.Time,
		})
	}
	return out, nil
}
