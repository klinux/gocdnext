package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// RunExists returns whether a run row is present. Used by the
// SSE log-stream handler — GetRunDetail would fetch stages + jobs
// + log tail, none of which we need just to validate the URL.
// Returns (false, nil) when the run is missing so the caller can
// distinguish "not found" from "DB error".
func (s *Store) RunExists(ctx context.Context, runID uuid.UUID) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM runs WHERE id = $1`, runID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: run exists: %w", err)
	}
	return true, nil
}

// RunIDForJobRun returns the owning run id for a job-run. Used by
// the SSE log-stream path, which needs to route live log events
// by run but only receives jobID from the agent message. Returns
// uuid.Nil + ErrJobRunNotFound if the row doesn't exist (e.g.
// the agent replays a log line for a job already wiped by the
// reaper — we swallow the publish in that case).
func (s *Store) RunIDForJobRun(ctx context.Context, jobRunID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT run_id FROM job_runs WHERE id = $1`,
		jobRunID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrJobRunNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: run id for job run: %w", err)
	}
	return id, nil
}

// InsertLogLine persists one log line. The ON CONFLICT clause makes retries
// harmless — if the agent re-sends the same (job_run_id, seq, at) after a
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

// BulkInsertLogLines persists a batch of lines in a single round-trip
// using a multi-VALUES INSERT — orders of magnitude less WAL and lock
// pressure than firing N separate InsertLogLine calls. ON CONFLICT
// preserves the dedup semantics InsertLogLine has on its own.
//
// Empty input is a no-op (agents flush on a timer; an idle window
// produces empty batches). At ~5 columns per row, Postgres' 65k
// parameter ceiling lets a single call carry up to ~13k lines —
// comfortably above any reasonable batch size.
func (s *Store) BulkInsertLogLines(ctx context.Context, lines []LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	const cols = 5
	args := make([]any, 0, len(lines)*cols)
	var sb strings.Builder
	sb.Grow(64 + len(lines)*40)
	sb.WriteString("INSERT INTO log_lines (job_run_id, seq, stream, at, text) VALUES ")
	now := time.Now().UTC()
	for i, l := range lines {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*cols + 1
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d)",
			base, base+1, base+2, base+3, base+4)
		at := l.At
		if at.IsZero() {
			at = now
		}
		args = append(args,
			pgUUID(l.JobRunID),
			l.Seq,
			l.Stream,
			pgtype.Timestamptz{Time: at, Valid: true},
			l.Text,
		)
	}
	sb.WriteString(" ON CONFLICT (job_run_id, seq, at) DO NOTHING")
	if _, err := s.pool.Exec(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("store: bulk insert log lines: %w", err)
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

	// StartedAt + FinishedAt let callers compute wall-clock
	// duration without a follow-up SELECT — the metrics package
	// observes here.
	StartedAt  *time.Time
	FinishedAt *time.Time

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
		StartedAt:  pgTimePtr(row.StartedAt),
		FinishedAt: pgTimePtr(row.FinishedAt),
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
		// Fail-fast: cancel remaining queued USER work (including
		// awaiting_approval gates — a rejected deploy upstream
		// means the downstream approvals are moot). The synthetic
		// `_notifications` stage is preserved on purpose so a
		// declared `on: failure` notification still fires. We
		// intentionally DO NOT complete the run yet — if the
		// pipeline has notifications they need to run first; the
		// run finalizes once `_notifications` drains (handled by
		// the run-progress branch below on the notifications
		// stage's own cascade tick).
		if err := q.CancelQueuedStagesInRun(ctx, runID); err != nil {
			return fmt.Errorf("store: cancel stages: %w", err)
		}
		if err := q.CancelQueuedJobsInRun(ctx, runID); err != nil {
			return fmt.Errorf("store: cancel jobs: %w", err)
		}
	}

	run, err := q.GetRunProgress(ctx, runID)
	if err != nil {
		return fmt.Errorf("store: run progress: %w", err)
	}
	if run.Unfinished > 0 {
		return nil
	}
	// Everything is done — user stages AND (if any) the
	// `_notifications` synth stage. Derive the run outcome from
	// USER stage jobs only so a notifier plugin failing doesn't
	// flip a passing build to failed or vice versa. The OLD path
	// keyed the aggregate on run.Failed which counted every
	// failed job including notifications; that's wrong once the
	// synth stage exists, and the new query excludes it cleanly.
	userOutcome, err := q.GetRunUserStageOutcome(ctx, runID)
	if err != nil {
		return fmt.Errorf("store: user stage outcome: %w", err)
	}
	runStatus := string(domain.StatusSuccess)
	if userOutcome.Failed > 0 {
		runStatus = string(domain.StatusFailed)
	}
	if err := q.CompleteRun(ctx, db.CompleteRunParams{
		ID: runID, Status: runStatus,
	}); err != nil {
		return fmt.Errorf("store: complete run: %w", err)
	}
	comp.RunCompleted = true
	comp.RunStatus = runStatus
	return nil
}

// UserStageOutcome is the aggregate terminal-state tally across
// a run's user stages only — the synthetic `_notifications`
// stage is excluded. The scheduler uses it to decide whether a
// notification's `on:` trigger matches before dispatching.
type UserStageOutcome struct {
	Failed   int64
	Canceled int64
}

// GetRunUserStageOutcome returns the aggregate tally of user-stage
// job outcomes for a run. Used by the scheduler's notification
// dispatch path; zeros on both fields = all user jobs succeeded.
func (s *Store) GetRunUserStageOutcome(ctx context.Context, runID uuid.UUID) (UserStageOutcome, error) {
	row, err := s.q.GetRunUserStageOutcome(ctx, pgUUID(runID))
	if err != nil {
		return UserStageOutcome{}, fmt.Errorf("store: user stage outcome: %w", err)
	}
	return UserStageOutcome{Failed: row.Failed, Canceled: row.Canceled}, nil
}

// NotificationTriggerMatches reports whether a notification's
// `on:` value fires given the user stages' aggregated outcome.
// Shared between the scheduler's dispatch path (decides whether
// to send the synth job to an agent) and tests.
func NotificationTriggerMatches(on domain.NotificationTrigger, o UserStageOutcome) bool {
	switch on {
	case domain.NotifyOnAlways:
		return true
	case domain.NotifyOnFailure:
		return o.Failed > 0
	case domain.NotifyOnCanceled:
		return o.Canceled > 0
	case domain.NotifyOnSuccess:
		return o.Failed == 0 && o.Canceled == 0
	default:
		return false
	}
}

// SkipNotificationJob marks a queued notification job as skipped
// and cascades — the stage can close once the last notification
// is either dispatched or skipped. Returns ok=false when the row
// wasn't in 'queued' (another tick raced us or the job already
// transitioned). The cascade pass lets the run complete cleanly
// even when every notification skipped (no dispatches at all).
func (s *Store) SkipNotificationJob(ctx context.Context, jobRunID uuid.UUID) (JobCompletion, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return JobCompletion{}, false, fmt.Errorf("store: skip job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.SkipJobRun(ctx, pgUUID(jobRunID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JobCompletion{}, false, nil
		}
		return JobCompletion{}, false, fmt.Errorf("store: skip job: %w", err)
	}

	comp := JobCompletion{
		JobRunID:   fromPgUUID(row.ID),
		RunID:      fromPgUUID(row.RunID),
		StageRunID: fromPgUUID(row.StageRunID),
		JobName:    row.Name,
	}
	if err := cascadeAfterJobCompletion(ctx, q, row.StageRunID, row.RunID, &comp); err != nil {
		return JobCompletion{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return JobCompletion{}, false, fmt.Errorf("store: skip job: commit: %w", err)
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
