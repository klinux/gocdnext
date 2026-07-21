package store

// The human decision path for a rollout approval gate (ADR-0001 Phase 2). This records
// votes + the terminal decision on the deploy_watch; it NEVER touches the cluster — the
// watcher actuates (Promote/Abort) next tick off gate_decision. Mirrors the hardened job
// approval engine (decideGate) but keys the gate on the deploy_watch, not job_runs.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

var (
	// ErrGateStale: the caller's view of the gate is stale — the gate isn't armed, its
	// gate_id no longer matches (the step was superseded / re-armed), or it was already
	// decided. The UI should re-fetch and vote against the current gate. Maps to 409.
	ErrGateStale = errors.New("store: rollout gate is stale — re-fetch the current gate")
	// ErrAlreadyVoted: this user already recorded a vote on this armed step; a second or
	// changed vote is rejected (the first binds — approve-then-reject can't terminalize).
	// Maps to 409.
	ErrAlreadyVoted = errors.New("store: already voted on this rollout gate")
)

// RolloutGateDecisionInput is one approve/reject on a deploy's armed step.
type RolloutGateDecisionInput struct {
	RevisionID uuid.UUID // the deploy_watch key (the /deploy-watches/{revID} route)
	GateID     uuid.UUID // the token the UI holds; must match the armed gate
	Decision   string    // "approved" | "rejected"
	UserID     uuid.UUID // the authenticated decider (required)
	User       string    // display label (name-preferred, for votes + audit)
	UserEmail  string    // for allow-list matching (OIDC name-vs-email)
	Comment    string
}

// RolloutGateResult reports the outcome. Decided=true means the gate became terminal
// this call (the watcher promotes/aborts next tick); PendingQuorum=true means a fresh
// approve was recorded but quorum isn't met yet (the window stays open).
type RolloutGateResult struct {
	Decided           bool
	Decision          string
	PendingQuorum     bool
	ApprovalsNow      int
	ApprovalsRequired int
}

// DecideRolloutGate records a vote and, when the decision becomes terminal (approve
// quorum met, or a fresh reject), stamps gate_decision + resumes the suspended deadline
// (deadline_at += now() - gate_armed_at, once) and writes the durable audit event — all
// in one tx under a FOR UPDATE lock on the deploy_watch row.
func (s *Store) DecideRolloutGate(ctx context.Context, in RolloutGateDecisionInput) (RolloutGateResult, error) {
	if in.Decision != "approved" && in.Decision != "rejected" {
		return RolloutGateResult{}, fmt.Errorf("store: rollout gate decision must be approved|rejected, got %q", in.Decision)
	}
	if in.RevisionID == uuid.Nil {
		return RolloutGateResult{}, fmt.Errorf("store: rollout gate decision: revision id required")
	}
	if in.UserID == uuid.Nil {
		return RolloutGateResult{}, fmt.Errorf("store: rollout gate decision: an authenticated user is required")
	}
	if in.GateID == uuid.Nil {
		return RolloutGateResult{}, ErrGateStale // no token → can't match an armed gate
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RolloutGateResult{}, fmt.Errorf("store: rollout gate begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the watch row FIRST (mirrors decideGate's FOR UPDATE OF j) so the gate-token
	// check, the vote, and the terminal flip all see one snapshot — deciders on the same
	// step serialize, closing the double-decide / orphan-vote window.
	var (
		gateID    pgtype.UUID
		decision  *string
		required  *int32
		approvers []string
		groups    []string
		jobRunID  pgtype.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT dw.gate_id, dw.gate_decision, dw.gate_required,
		       dw.gate_approvers, dw.gate_approver_groups, dr.job_run_id
		FROM deploy_watches dw
		JOIN deployment_revisions dr ON dr.id = dw.deployment_revision_id
		WHERE dw.deployment_revision_id = $1
		FOR UPDATE OF dw
	`, in.RevisionID).Scan(&gateID, &decision, &required, &approvers, &groups, &jobRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RolloutGateResult{}, ErrGateStale // watch gone (finalized) or unknown revision
	}
	if err != nil {
		return RolloutGateResult{}, fmt.Errorf("store: load rollout gate: %w", err)
	}

	// Gate-token check: armed (gate_id set), matching the caller's token, and not yet
	// decided. Any mismatch = stale (re-fetch). All three collapse to one 409 so a stale
	// UI can't distinguish "not armed" from "re-armed" from "already decided".
	if fromPgUUID(gateID) != in.GateID || decision != nil {
		return RolloutGateResult{}, ErrGateStale
	}
	if !jobRunID.Valid {
		return RolloutGateResult{}, fmt.Errorf("store: rollout gate has no job run to record votes against")
	}
	req := int32(1)
	if required != nil && *required > 1 {
		req = *required
	}

	// Allow-list (same semantics as the job gate): user in `approvers` by name OR email,
	// OR in one of `approver_groups`. Empty both = any authenticated user.
	if len(approvers) > 0 || len(groups) > 0 {
		allowed := (in.User != "" && slices.Contains(approvers, in.User)) ||
			(in.UserEmail != "" && slices.Contains(approvers, in.UserEmail))
		if !allowed && len(groups) > 0 {
			names, gerr := s.q.WithTx(tx).ListUserGroupNames(ctx, pgUUID(in.UserID))
			if gerr != nil {
				return RolloutGateResult{}, fmt.Errorf("store: load user groups: %w", gerr)
			}
			for _, n := range names {
				if slices.Contains(groups, n) {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			return RolloutGateResult{}, ErrApproverNotAllowed
		}
	}

	// Record the vote — FRESH only. Unlike the job gate (idempotent re-post), a rollout
	// step binds the first vote: a duplicate or CHANGED vote (approve-then-reject) is a
	// 409, so a user can't flip their vote to force-terminalize.
	tag, err := tx.Exec(ctx, `
		INSERT INTO job_run_approvals (job_run_id, user_id, user_label, decision, comment)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (job_run_id, user_id) DO NOTHING
	`, jobRunID, pgUUID(in.UserID), in.User, in.Decision, in.Comment)
	if err != nil {
		return RolloutGateResult{}, fmt.Errorf("store: record rollout vote: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return RolloutGateResult{}, ErrAlreadyVoted
	}

	// Approve accumulates toward quorum; a partial approve is NOT terminal (commit the
	// vote, keep the window open). A fresh reject is immediately terminal.
	approvedNow := int32(0)
	if in.Decision == "approved" {
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM job_run_approvals WHERE job_run_id = $1 AND decision = 'approved'`,
			jobRunID).Scan(&approvedNow); err != nil {
			return RolloutGateResult{}, fmt.Errorf("store: count rollout approvals: %w", err)
		}
		if approvedNow < req {
			if err := tx.Commit(ctx); err != nil {
				return RolloutGateResult{}, fmt.Errorf("store: rollout gate commit: %w", err)
			}
			return RolloutGateResult{PendingQuorum: true, ApprovalsNow: int(approvedNow), ApprovalsRequired: int(req)}, nil
		}
	}

	// Terminal: stamp the decision AND resume the deadline once (deadline_at += now() -
	// gate_armed_at). Guarded on gate_decision IS NULL + the matching gate_id so a
	// concurrent decider (or a re-armed step) can't double-shift or clobber.
	dtag, err := tx.Exec(ctx, `
		UPDATE deploy_watches
		SET gate_decision  = $2,
		    gate_decided_by = $3,
		    gate_decided_at = NOW(),
		    deadline_at    = deadline_at + (NOW() - gate_armed_at)
		WHERE deployment_revision_id = $1 AND gate_id = $4 AND gate_decision IS NULL
	`, in.RevisionID, in.Decision, in.User, in.GateID)
	if err != nil {
		return RolloutGateResult{}, fmt.Errorf("store: stamp rollout decision: %w", err)
	}
	if dtag.RowsAffected() == 0 {
		return RolloutGateResult{}, ErrGateStale // a concurrent decider won the flip
	}

	if err := s.emitRolloutGateAudit(ctx, tx, in, jobRunID); err != nil {
		return RolloutGateResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RolloutGateResult{}, fmt.Errorf("store: rollout gate commit: %w", err)
	}
	return RolloutGateResult{Decided: true, Decision: in.Decision, ApprovalsNow: int(approvedNow), ApprovalsRequired: int(req)}, nil
}

// emitRolloutGateAudit writes the durable per-step record IN THE SAME TX — it captures
// the gate_id, decision + decider, and every vote, because ClearRolloutGate later deletes
// the transient job_run_approvals rows.
func (s *Store) emitRolloutGateAudit(ctx context.Context, tx pgx.Tx, in RolloutGateDecisionInput, jobRunID pgtype.UUID) error {
	rows, err := tx.Query(ctx,
		`SELECT user_label, decision FROM job_run_approvals WHERE job_run_id = $1 ORDER BY decided_at`, jobRunID)
	if err != nil {
		return fmt.Errorf("store: gather rollout votes for audit: %w", err)
	}
	type vote struct {
		User     string `json:"user"`
		Decision string `json:"decision"`
	}
	var votes []vote
	for rows.Next() {
		var v vote
		if err := rows.Scan(&v.User, &v.Decision); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan rollout vote: %w", err)
		}
		votes = append(votes, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: gather rollout votes: %w", err)
	}

	meta, err := json.Marshal(map[string]any{
		"gate_id":  in.GateID.String(),
		"decision": in.Decision,
		"votes":    votes,
	})
	if err != nil {
		return fmt.Errorf("store: marshal rollout audit metadata: %w", err)
	}
	action := AuditActionRolloutGateApprove
	if in.Decision == "rejected" {
		action = AuditActionRolloutGateReject
	}
	if _, err := s.q.WithTx(tx).InsertAuditEvent(ctx, db.InsertAuditEventParams{
		ActorID:    nullableUUID(in.UserID),
		ActorEmail: in.UserEmail,
		Action:     action,
		TargetType: "deploy_watch",
		TargetID:   in.RevisionID.String(),
		Metadata:   meta,
	}); err != nil {
		return fmt.Errorf("store: emit rollout gate audit: %w", err)
	}
	return nil
}
