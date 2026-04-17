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

// LogLine is the write-side shape for agent-produced log lines.
type LogLine struct {
	JobRunID uuid.UUID
	Seq      int64
	Stream   string
	At       time.Time
	Text     string
}

// InsertLogLine persists one log line. The ON CONFLICT clause makes retries
// harmless — if the agent re-sends the same (job_run_id, seq) after a
// disconnect, we keep the first copy.
func (s *Store) InsertLogLine(ctx context.Context, in LogLine) error {
	at := in.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	err := s.q.InsertLogLine(ctx, db.InsertLogLineParams{
		JobRunID: pgUUID(in.JobRunID),
		Seq:      in.Seq,
		Stream:   in.Stream,
		At:       pgtype.Timestamptz{Time: at, Valid: true},
		Text:     in.Text,
	})
	if err != nil {
		return fmt.Errorf("store: insert log line: %w", err)
	}
	return nil
}

// CompleteJobInput captures the terminal payload coming off an agent's
// JobResult. Status must be "success" or "failed".
type CompleteJobInput struct {
	JobRunID uuid.UUID
	Status   string
	ExitCode int32
	ErrorMsg string
}

// JobCompletion summarises the cascade that CompleteJob kicked off: which
// stage/run progressed, their terminal status, and the agent id so the
// caller can release capacity.
type JobCompletion struct {
	JobRunID   uuid.UUID
	RunID      uuid.UUID
	StageRunID uuid.UUID
	AgentID    uuid.UUID
	JobName    string

	StageCompleted bool
	StageStatus    string

	RunCompleted bool
	RunStatus    string
}

// CompleteJob flips one job_run to its terminal state and cascades into the
// stage (all jobs done → promote) and the run (all stages done → promote). If
// a stage fails, the run is marked failed and the remaining queued stages /
// jobs are canceled so the scheduler stops dispatching. Returns ok=false when
// the job was not in 'running' (duplicate result, already terminal).
func (s *Store) CompleteJob(ctx context.Context, in CompleteJobInput) (JobCompletion, bool, error) {
	if in.Status != string(domain.StatusSuccess) && in.Status != string(domain.StatusFailed) {
		return JobCompletion{}, false, fmt.Errorf("store: complete job: invalid status %q", in.Status)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return JobCompletion{}, false, fmt.Errorf("store: complete job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	exitCode := in.ExitCode
	row, err := q.CompleteJobRun(ctx, db.CompleteJobRunParams{
		ID:       pgUUID(in.JobRunID),
		Status:   in.Status,
		ExitCode: &exitCode,
		Error:    nullableString(in.ErrorMsg),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JobCompletion{}, false, nil
		}
		return JobCompletion{}, false, fmt.Errorf("store: complete job: %w", err)
	}

	comp := JobCompletion{
		JobRunID:   fromPgUUID(row.ID),
		RunID:      fromPgUUID(row.RunID),
		StageRunID: fromPgUUID(row.StageRunID),
		AgentID:    fromPgUUID(row.AgentID),
		JobName:    row.Name,
	}

	stage, err := q.GetStageProgress(ctx, row.StageRunID)
	if err != nil {
		return JobCompletion{}, false, fmt.Errorf("store: stage progress: %w", err)
	}

	if stage.Unfinished == 0 {
		stageStatus := string(domain.StatusSuccess)
		if stage.Failed > 0 {
			stageStatus = string(domain.StatusFailed)
		}
		if err := q.CompleteStageRun(ctx, db.CompleteStageRunParams{
			ID: row.StageRunID, Status: stageStatus,
		}); err != nil {
			return JobCompletion{}, false, fmt.Errorf("store: complete stage: %w", err)
		}
		comp.StageCompleted = true
		comp.StageStatus = stageStatus

		if stageStatus == string(domain.StatusFailed) {
			// Fail-fast: cancel remaining queued work and mark the run failed.
			if err := q.CancelQueuedStagesInRun(ctx, row.RunID); err != nil {
				return JobCompletion{}, false, fmt.Errorf("store: cancel stages: %w", err)
			}
			if err := q.CancelQueuedJobsInRun(ctx, row.RunID); err != nil {
				return JobCompletion{}, false, fmt.Errorf("store: cancel jobs: %w", err)
			}
			if err := q.CompleteRun(ctx, db.CompleteRunParams{
				ID: row.RunID, Status: string(domain.StatusFailed),
			}); err != nil {
				return JobCompletion{}, false, fmt.Errorf("store: complete run: %w", err)
			}
			comp.RunCompleted = true
			comp.RunStatus = string(domain.StatusFailed)
		} else {
			run, err := q.GetRunProgress(ctx, row.RunID)
			if err != nil {
				return JobCompletion{}, false, fmt.Errorf("store: run progress: %w", err)
			}
			if run.Unfinished == 0 {
				runStatus := string(domain.StatusSuccess)
				if run.Failed > 0 {
					runStatus = string(domain.StatusFailed)
				}
				if err := q.CompleteRun(ctx, db.CompleteRunParams{
					ID: row.RunID, Status: runStatus,
				}); err != nil {
					return JobCompletion{}, false, fmt.Errorf("store: complete run: %w", err)
				}
				comp.RunCompleted = true
				comp.RunStatus = runStatus
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return JobCompletion{}, false, fmt.Errorf("store: complete job: commit: %w", err)
	}
	return comp, true, nil
}

// NotifyRunQueued emits `run_queued` so the scheduler wakes up for the given
// run. Used after a stage completes successfully to advance to the next one
// without waiting for the periodic tick.
func (s *Store) NotifyRunQueued(ctx context.Context, runID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, "SELECT pg_notify($1, $2)", RunQueuedChannel, runID.String())
	if err != nil {
		return fmt.Errorf("store: notify run_queued: %w", err)
	}
	return nil
}
