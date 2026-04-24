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

	if err := cascadeAfterJobCompletion(ctx, q, row.StageRunID, row.RunID, &comp); err != nil {
		return JobCompletion{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return JobCompletion{}, false, fmt.Errorf("store: complete job: commit: %w", err)
	}
	return comp, true, nil
}

// cascadeAfterJobCompletion promotes the stage (and cascades into
// the run) once a job lands in a terminal state. Shared between
// CompleteJob (agent-driven) and the approval gate transitions
// (human-driven) so both paths hit the same fail-fast + final-
// promotion logic. Callers must pass the same pgx transaction
// handle (q := s.q.WithTx(tx)) so rollback on error wipes the
// partially-stamped stage/run rows.
func cascadeAfterJobCompletion(ctx context.Context, q *db.Queries, stageRunID, runID pgtype.UUID, comp *JobCompletion) error {
	stage, err := q.GetStageProgress(ctx, stageRunID)
	if err != nil {
		return fmt.Errorf("store: stage progress: %w", err)
	}
	if stage.Unfinished > 0 {
		return nil
	}

	stageStatus := string(domain.StatusSuccess)
	if stage.Failed > 0 {
		stageStatus = string(domain.StatusFailed)
	}
	if err := q.CompleteStageRun(ctx, db.CompleteStageRunParams{
		ID: stageRunID, Status: stageStatus,
	}); err != nil {
		return fmt.Errorf("store: complete stage: %w", err)
	}
	comp.StageCompleted = true
	comp.StageStatus = stageStatus

	if stageStatus == string(domain.StatusFailed) {
		// Fail-fast: cancel remaining queued work (including
		// awaiting_approval gates — a rejected deploy upstream
		// means the downstream approvals are moot) and mark the
		// run failed.
		if err := q.CancelQueuedStagesInRun(ctx, runID); err != nil {
			return fmt.Errorf("store: cancel stages: %w", err)
		}
		if err := q.CancelQueuedJobsInRun(ctx, runID); err != nil {
			return fmt.Errorf("store: cancel jobs: %w", err)
		}
		if err := q.CompleteRun(ctx, db.CompleteRunParams{
			ID: runID, Status: string(domain.StatusFailed),
		}); err != nil {
			return fmt.Errorf("store: complete run: %w", err)
		}
		comp.RunCompleted = true
		comp.RunStatus = string(domain.StatusFailed)
		return nil
	}

	run, err := q.GetRunProgress(ctx, runID)
	if err != nil {
		return fmt.Errorf("store: run progress: %w", err)
	}
	if run.Unfinished == 0 {
		runStatus := string(domain.StatusSuccess)
		if run.Failed > 0 {
			runStatus = string(domain.StatusFailed)
		}
		if err := q.CompleteRun(ctx, db.CompleteRunParams{
			ID: runID, Status: runStatus,
		}); err != nil {
			return fmt.Errorf("store: complete run: %w", err)
		}
		comp.RunCompleted = true
		comp.RunStatus = runStatus
	}
	return nil
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
