package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// ReclaimAction names the reaper's verdict for a stale job.
type ReclaimAction string

const (
	ReclaimActionRequeued ReclaimAction = "requeued"
	ReclaimActionFailed   ReclaimAction = "failed"
	ReclaimActionSkipped  ReclaimAction = "skipped"
)

// ReclaimResult is the per-job summary returned by ReclaimStaleJobs.
type ReclaimResult struct {
	JobRunID uuid.UUID
	RunID    uuid.UUID
	JobName  string
	AgentID  uuid.UUID
	Attempt  int32
	Action   ReclaimAction
	Err      error
}

// MarkAgentSeen bumps agents.last_seen_at. Called from the heartbeat handler
// so the reaper can distinguish live agents (with recent heartbeats) from
// zombies whose TCP stream is still open but the process is hung.
func (s *Store) MarkAgentSeen(ctx context.Context, agentID uuid.UUID) error {
	if err := s.q.UpdateAgentLastSeen(ctx, pgUUID(agentID)); err != nil {
		return fmt.Errorf("store: mark agent seen: %w", err)
	}
	return nil
}

// ReclaimStaleJobs walks every running job whose agent is offline or quiet
// beyond `staleness`, and either re-queues (when attempt < maxAttempts) or
// fails (via CompleteJob, so stage/run cascade still kicks in).
//
// Returns one entry per acted-on job; errors are attached per-entry so a
// single bad row doesn't abort the whole sweep.
func (s *Store) ReclaimStaleJobs(ctx context.Context, maxAttempts int32, staleness time.Duration) ([]ReclaimResult, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if staleness <= 0 {
		staleness = 90 * time.Second
	}

	stale, err := s.q.ListStaleRunningJobs(ctx, intervalFor(staleness))
	if err != nil {
		return nil, fmt.Errorf("store: list stale jobs: %w", err)
	}

	out := make([]ReclaimResult, 0, len(stale))
	for _, j := range stale {
		res := ReclaimResult{
			JobRunID: fromPgUUID(j.ID),
			RunID:    fromPgUUID(j.RunID),
			JobName:  j.Name,
			AgentID:  fromPgUUID(j.AgentID),
			Attempt:  j.Attempt,
		}

		if j.Attempt+1 > maxAttempts {
			// Cap reached — fail the job via CompleteJob so stage/run cascade
			// matches the legacy failure path.
			_, ok, err := s.CompleteJob(ctx, CompleteJobInput{
				JobRunID: res.JobRunID,
				Status:   string(domain.StatusFailed),
				ExitCode: -1,
				ErrorMsg: fmt.Sprintf("agent lost before completion (attempts=%d, max=%d)", j.Attempt, maxAttempts),
			})
			switch {
			case err != nil:
				res.Err = fmt.Errorf("fail at max: %w", err)
			case !ok:
				res.Action = ReclaimActionSkipped
			default:
				res.Action = ReclaimActionFailed
			}
			out = append(out, res)
			continue
		}

		// Under the cap — attempt an atomic re-queue.
		if err := s.requeueStaleJob(ctx, res.JobRunID, maxAttempts, &res); err != nil {
			res.Err = err
		}
		out = append(out, res)
	}
	return out, nil
}

func (s *Store) requeueStaleJob(ctx context.Context, jobID uuid.UUID, maxAttempts int32, res *ReclaimResult) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.ReclaimJobForRetry(ctx, db.ReclaimJobForRetryParams{
		ID: pgUUID(jobID), MaxAttempts: maxAttempts,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Another sweeper tick won, or status flipped out from under us.
			res.Action = ReclaimActionSkipped
			return nil
		}
		return fmt.Errorf("reclaim: %w", err)
	}

	if err := q.DeleteLogLinesByJob(ctx, row.ID); err != nil {
		return fmt.Errorf("delete logs: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", RunQueuedChannel, fromPgUUID(row.RunID).String()); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	res.Action = ReclaimActionRequeued
	res.Attempt = row.Attempt
	return nil
}

func intervalFor(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
