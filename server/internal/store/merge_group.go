package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// MergeGroupCanceledRunChannel is the NOTIFY channel emitted after a
// merge_group.destroyed delivery cancels active runs for the abandoned queue
// SHA. The scheduler consumes it to push CancelJob frames and service cleanup.
const MergeGroupCanceledRunChannel = "run_merge_group_canceled"

// CancelMergeGroupRuns cancels every active merge-group run for the provided
// GitHub merge_group.head_sha and emits one transactional NOTIFY per canceled
// run. Idempotent: a redelivery after the first transaction commits finds no
// active rows and returns an empty list.
func (s *Store) CancelMergeGroupRuns(ctx context.Context, headSHA, reason string) ([]uuid.UUID, error) {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return nil, errors.New("store: merge group head sha required")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("store: cancel merge group: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	rows, err := tx.Query(ctx, `
		SELECT id
		FROM runs
		WHERE cause = $1
		  AND status IN ('queued', 'running')
		  AND cause_detail->>'mg_head_sha' = $2
		FOR UPDATE
	`, string(domain.CauseMergeGroup), headSHA)
	if err != nil {
		return nil, fmt.Errorf("store: cancel merge group: candidates: %w", err)
	}
	var runIDs []uuid.UUID
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: cancel merge group: scan: %w", err)
		}
		runIDs = append(runIDs, fromPgUUID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: cancel merge group: rows: %w", err)
	}
	rows.Close()

	cancelReason := "github merge_group destroyed"
	if reason = strings.TrimSpace(reason); reason != "" {
		cancelReason += ": " + reason
	}
	for _, runID := range runIDs {
		if _, err := tx.Exec(ctx, `
			UPDATE job_runs j
			SET status = CASE WHEN j.status = 'running' THEN j.status ELSE 'canceled' END,
			    finished_at = CASE WHEN j.status = 'running'
			                       THEN j.finished_at ELSE COALESCE(j.finished_at, NOW()) END,
			    cancel_requested_at = CASE WHEN j.status = 'running'
			                              THEN COALESCE(j.cancel_requested_at, NOW())
			                              ELSE j.cancel_requested_at END,
			    cancel_origin = COALESCE(j.cancel_origin, $2)
			FROM stage_runs s
			WHERE j.run_id = $1
			  AND s.id = j.stage_run_id
			  AND s.name != '_notifications'
			  AND j.status IN ('queued', 'running', 'awaiting_approval')
		`, pgUUID(runID), string(CancelOriginMergeGroup)); err != nil {
			return nil, fmt.Errorf("store: cancel merge group: jobs: %w", err)
		}
		if err := q.CancelQueuedStagesInRun(ctx, pgUUID(runID)); err != nil {
			return nil, fmt.Errorf("store: cancel merge group: stages: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE runs
			SET status = 'canceled',
			    finished_at = COALESCE(finished_at, NOW()),
			    queue_reason = NULL,
			    cancel_reason = $2,
			    merge_group_cancel_effects_claimed_at = NULL,
			    merge_group_cancel_effects_at = NULL
			WHERE id = $1
			  AND status IN ('queued', 'running')
		`, pgUUID(runID), cancelReason)
		if err != nil {
			return nil, fmt.Errorf("store: cancel merge group: run: %w", err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, MergeGroupCanceledRunChannel, runID.String()); err != nil {
			return nil, fmt.Errorf("store: cancel merge group: notify: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: cancel merge group: commit: %w", err)
	}
	return runIDs, nil
}

// ClaimMergeGroupCancelEffects atomically claims the right to fire external
// effects for a merge-group-canceled run. A stale claim is reclaimable after the
// same lease used by supersede effects.
func (s *Store) ClaimMergeGroupCancelEffects(ctx context.Context, runID uuid.UUID) (claimed, firstClaim bool, err error) {
	var first bool
	err = s.pool.QueryRow(ctx, `
		WITH cur AS (
		    SELECT id,
		           merge_group_cancel_effects_claimed_at AS prev_claim,
		           merge_group_cancel_effects_at AS effects_at
		    FROM runs
		    WHERE id = $1
		    FOR UPDATE
		)
		UPDATE runs r
		SET merge_group_cancel_effects_claimed_at = NOW()
		FROM cur
		WHERE r.id = cur.id
		  AND r.cause = $2
		  AND r.status = 'canceled'
		  AND cur.effects_at IS NULL
		  AND (cur.prev_claim IS NULL
		       OR cur.prev_claim < NOW() - $3::INTERVAL)
		RETURNING (cur.prev_claim IS NULL)::boolean
	`, pgUUID(runID), string(domain.CauseMergeGroup), pgtype.Interval{Microseconds: supersedeEffectsLease.Microseconds(), Valid: true}).Scan(&first)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("store: claim merge group cancel effects: %w", err)
	}
	return true, first, nil
}

func (s *Store) MarkMergeGroupCancelEffectsDone(ctx context.Context, runID uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE runs
		SET merge_group_cancel_effects_at = NOW()
		WHERE id = $1
		  AND cause = $2
		  AND status = 'canceled'
	`, pgUUID(runID), string(domain.CauseMergeGroup)); err != nil {
		return fmt.Errorf("store: mark merge group cancel effects done: %w", err)
	}
	return nil
}

func (s *Store) ListPendingMergeGroupCancelEffects(ctx context.Context, max int32) ([]uuid.UUID, error) {
	if max <= 0 {
		max = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id
		FROM runs
		WHERE cause = $1
		  AND status = 'canceled'
		  AND merge_group_cancel_effects_at IS NULL
		  AND (merge_group_cancel_effects_claimed_at IS NULL
		       OR merge_group_cancel_effects_claimed_at < NOW() - $2::INTERVAL)
		ORDER BY finished_at
		LIMIT $3
	`, string(domain.CauseMergeGroup), pgtype.Interval{Microseconds: supersedeEffectsLease.Microseconds(), Valid: true}, max)
	if err != nil {
		return nil, fmt.Errorf("store: list pending merge group cancel effects: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0)
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: list pending merge group cancel effects scan: %w", err)
		}
		out = append(out, fromPgUUID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list pending merge group cancel effects rows: %w", err)
	}
	return out, nil
}

// MergeGroupCanceledRunServiceGeneration returns the service_generation only
// while the run is still a canceled merge-group run with unresolved effects.
// A rerun revive flips status back to running and bumps generation in one
// transaction, so this single read prevents stale cleanup from touching revived
// services.
func (s *Store) MergeGroupCanceledRunServiceGeneration(ctx context.Context, runID uuid.UUID) (generation int64, canceled bool, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT service_generation
		FROM runs
		WHERE id = $1
		  AND cause = $2
		  AND status = 'canceled'
		  AND merge_group_cancel_effects_at IS NULL
	`, pgUUID(runID), string(domain.CauseMergeGroup)).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: merge group canceled run generation: %w", err)
	}
	return generation, true, nil
}

// RunCanceledByMergeGroup reports whether a run was canceled because GitHub
// destroyed its merge-group ref. The checks reporter uses this to suppress a
// terminal red/cancelled check on a SHA GitHub has abandoned.
func (s *Store) RunCanceledByMergeGroup(ctx context.Context, runID uuid.UUID) (bool, error) {
	var yes bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM runs r
		    WHERE r.id = $1
		      AND r.cause = $2
		      AND r.status = 'canceled'
		      AND (
		        r.cancel_reason LIKE 'github merge_group destroyed%'
		        OR EXISTS (
		            SELECT 1 FROM job_runs j
		            WHERE j.run_id = r.id AND j.cancel_origin = $3
		        )
		      )
		)
	`, pgUUID(runID), string(domain.CauseMergeGroup), string(CancelOriginMergeGroup)).Scan(&yes); err != nil {
		return false, fmt.Errorf("store: run canceled by merge group: %w", err)
	}
	return yes, nil
}
