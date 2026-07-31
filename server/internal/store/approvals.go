package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// ErrApprovalSuperseded signals the gate is no longer pending because its RUN was
// superseded (a newer revision won the lane) — distinct from a normal
// already-decided gate. Callers map it to 409 with a superseded-specific message so
// the UI can say "this run was superseded" rather than "already decided". Detected
// under the gate row lock BEFORE recording a vote, so a superseded gate never
// accrues an orphan vote (#97 Phase 2).
var ErrApprovalSuperseded = errors.New("store: approval gate's run was superseded")

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
	// email as fallback. Recorded as the vote's user_label and
	// matched against the gate's approvers array (string-compare),
	// distinct from UserID.
	User string
	// UserEmail is the authenticated user's email, matched against the
	// gate's approvers array IN ADDITION to User. Under OIDC the `name`
	// claim becomes User (e.g. "Kleber Rocha"), so a gate listing the
	// stable email/username would never match if we only compared User
	// (#51). Empty in anonymous/dev mode.
	UserEmail string
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

	PendingQuorum     bool
	ApprovalsNow      int
	ApprovalsRequired int
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

	// The read/lock sequence below follows the MANDATORY GLOBAL LOCK ORDER
	// documented on store.ProjectEnvFreezeLockKey:
	//
	//   (1) LaneEnvLockKey(s), name-ordered   (2) ProjectEnvFreezeLockKey(s),
	//   name-ordered                          (3) row FOR UPDATE
	//
	// which is why the gate row is first read WITHOUT a row lock: the lane and
	// freeze keys have to be taken before any FOR UPDATE, and computing the lane
	// key needs the run's pipeline/ref/mode. Dispatch takes lane-then-freeze; if
	// approval took freeze-then-lane (which is what happens if the freeze check
	// sits after the FOR UPDATE, since writeGatePassMarkers takes the lane lock
	// at the very end) the two invert and deadlock — undetectably, because the
	// lane lock is session-level on another connection.
	//
	// EVERYTHING below runs on q.WithTx(tx), never on s.q: a second pooled
	// connection acquired while this tx holds locks self-deadlocks outright at
	// MaxConns=1 and burns a connection at any pool size.
	var (
		gate           bool
		status         string
		approvers      []string
		approverGroups []string
		required       int32
		parentRun      pgtype.UUID
		stageRunID     pgtype.UUID
		gateName       string
		runSuperseded  bool
		projectID      pgtype.UUID
		pipelineID     pgtype.UUID
		runRef         string
		runDefinition  []byte
	)
	// Plain SELECT, no FOR UPDATE — see the lock-order note above. runs carries
	// no project_id (only pipeline_id), so the project comes through pipelines.
	// r.definition is the run's own snapshot: the gate's governed envs must be
	// read from what this run was created with, not from a pipeline row that may
	// have been re-applied since.
	err = tx.QueryRow(ctx, `
		SELECT j.approval_gate, j.status, j.approvers, j.approver_groups, j.approval_required,
		       j.run_id, j.stage_run_id, j.name, (r.superseded_by IS NOT NULL),
		       p.project_id, r.pipeline_id, r.ref, r.definition
		FROM job_runs j
		JOIN runs r ON r.id = j.run_id
		JOIN pipelines p ON p.id = r.pipeline_id
		WHERE j.id = $1
	`, d.JobRunID).Scan(&gate, &status, &approvers, &approverGroups, &required, &parentRun, &stageRunID,
		&gateName, &runSuperseded, &projectID, &pipelineID, &runRef, &runDefinition)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApprovalResult{}, ErrApprovalGateNotFound
	}
	if err != nil {
		return ApprovalResult{}, fmt.Errorf("store: load approval row: %w", err)
	}
	if !gate {
		return ApprovalResult{}, ErrApprovalGateNotFound
	}
	// The pending/superseded checks are REPEATED under the row lock further down
	// — this early copy only avoids paying for locks on an obviously-decided
	// gate. The authoritative revalidation is the locked one.
	if status != "awaiting_approval" {
		// A superseded run's gates are canceled by the supersede terminalizer; report
		// that distinctly (and BEFORE any vote insert, so no orphan vote accrues).
		if runSuperseded {
			return ApprovalResult{}, ErrApprovalSuperseded
		}
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
	//
	// Runs BEFORE any lock: an unauthorized approver must not be able to make
	// the server take advisory locks on a production environment, however
	// briefly.
	if len(approvers) > 0 || len(approverGroups) > 0 {
		// Match the approvers list against EITHER the display label
		// (User — name-preferred) OR the email. Under OIDC the `name`
		// claim wins as User, so a gate listing the stable email would
		// never match on User alone (#51). Groups (UserID) below stay
		// the most robust path, immune to name/email string drift.
		allowed := (d.User != "" && slices.Contains(approvers, d.User)) ||
			(d.UserEmail != "" && slices.Contains(approvers, d.UserEmail))
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

	// Locks, in the mandatory global order. ONLY on approve: a rejection cannot
	// promote anything to a frozen environment, so it pays for neither the
	// lane/freeze locks nor the freeze query — it only takes the common row lock
	// below. Making reject wait on a frozen env's lock would also be actively
	// wrong: "stop this from shipping" is exactly what you still want to be able
	// to do during a change-freeze.
	var governedEnvs []string
	if decision == "approved" {
		governedEnvs, err = lockApprovalEnvs(ctx, tx, fromPgUUID(projectID), fromPgUUID(pipelineID),
			runRef, gateName, runDefinition)
		if err != nil {
			return ApprovalResult{}, err
		}
	}

	// NOW the row lock (step 3 of the order). FOR UPDATE OF j locks ONLY the gate
	// row (not the run) so the gate's status can't flip under us between here and
	// the vote/flip below — closing the orphan-vote window where a concurrent
	// supersede cancels the gate after we read it as pending. Locking only the job
	// row keeps the global order job_runs -> (nothing here) intact; we deliberately
	// do NOT lock runs (that would invert the job->runs order the result cascade
	// uses and could deadlock). Common to approve AND reject.
	if err := tx.QueryRow(ctx, `
		SELECT j.approval_gate, j.status, (r.superseded_by IS NOT NULL)
		FROM job_runs j
		JOIN runs r ON r.id = j.run_id
		WHERE j.id = $1
		FOR UPDATE OF j
	`, d.JobRunID).Scan(&gate, &status, &runSuperseded); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApprovalResult{}, ErrApprovalGateNotFound
		}
		return ApprovalResult{}, fmt.Errorf("store: revalidate approval row: %w", err)
	}
	// approval_gate is re-read under the lock, not just status: the unlocked
	// read above happened before any lock was taken, and defence in depth here
	// costs one already-fetched column. A row that stopped being a gate between
	// the two reads must not receive an approval decision.
	if !gate {
		return ApprovalResult{}, ErrApprovalGateNotFound
	}
	if status != "awaiting_approval" {
		if runSuperseded {
			return ApprovalResult{}, ErrApprovalSuperseded
		}
		return ApprovalResult{}, ErrApprovalNotPending
	}

	// The freeze check itself, under the locks taken above and on the tx-bound
	// queries handle. Locking (not merely reading) is what serialises an approve
	// against a concurrent freeze: whichever takes the lock first wins, and a
	// freeze that lost the race applies to the NEXT approval. It is also a
	// backstop over the dispatch seam — an approval that slipped through here
	// would still be refused at admission.
	if decision == "approved" && len(governedEnvs) > 0 {
		frozen, ferr := frozenGovernedEnvsTx(ctx, s.q.WithTx(tx), fromPgUUID(projectID), governedEnvs)
		if ferr != nil {
			return ApprovalResult{}, ferr
		}
		if len(frozen) > 0 {
			// Name EVERY frozen env, not just the first: the operator otherwise
			// unfreezes one, retries, and gets refused again by the next.
			return ApprovalResult{}, fmt.Errorf("%w: %s", ErrEnvironmentFrozen, strings.Join(frozen, ", "))
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
	// #97 Phase 2: on approval, record the gate-pass marker for every concrete env
	// this gate now clears (once all its governing gates passed), under the lane-env
	// advisory lock. Fail-closed — a supersede pipeline must not approve a deploy
	// without the backstop marker. No-op on reject (nextStatus='failed') and for
	// supersede=off. Runs BEFORE the cascade so the marker lands with the flip.
	if decision == "approved" {
		if err := s.writeGatePassMarkers(ctx, tx, gateName, fromPgUUID(parentRun)); err != nil {
			return ApprovalResult{}, err
		}
	}

	q := s.q.WithTx(tx)
	var comp JobCompletion
	if err := cascadeAfterJobCompletion(ctx, q, stageRunID, parentRun, &comp); err != nil {
		return ApprovalResult{}, err
	}

	// #97 cascade supersede fire: approving a gate can complete its stage and make
	// the NEXT stage's gate reachable (a gate chain), clearing older lane siblings
	// pending for that gate's env. Same-tx, effects via the run_superseded NOTIFY.
	// A rejection fails the stage, so supersedeAfterCascade's success-only guard
	// makes it a no-op there.
	if _, err := s.supersedeAfterCascade(ctx, tx, fromPgUUID(parentRun), fromPgUUID(stageRunID), &comp); err != nil {
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
