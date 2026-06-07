package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
//
// CAUTION: this entrypoint has NO snapshot-CAS — anything calling
// it can land lines on a job_run regardless of who currently owns
// it. The live agent log path uses BulkInsertLogLinesForJob below
// instead. This raw variant is kept for tests / retention / archive
// callers that operate on detached rows where ownership isn't the
// relevant invariant.
func (s *Store) BulkInsertLogLines(ctx context.Context, lines []LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	return bulkInsertLogLinesQ(ctx, s.pool, lines)
}

// BulkInsertLogLinesForJob is the snapshot-validating log-write
// path the live agent stream uses. Locks the job_run row FOR UPDATE
// inside a transaction, verifies (agent_id, attempt) still matches
// the snapshot the caller captured at dispatch time, then inserts
// the batch. Returns ErrSnapshotStale when the row has been
// reclaimed / redispatched out from under us — the caller drops
// the batch rather than letting a dying stream poison the next
// attempt's logs.
//
// Same race the test_results path closes: an old stream alive past
// a reaper-driven requeue (whose DeleteLogLinesByJob just cleared
// the row) could otherwise repopulate log_lines, which would then
// either look stale alongside the new attempt's logs OR win the
// (job_run_id, seq, at) ON CONFLICT race and silently drop the
// new attempt's legitimate lines.
//
// All lines in `lines` MUST share the given jobID — the caller
// (batcher) groups by job_run_id before calling.
func (s *Store) BulkInsertLogLinesForJob(
	ctx context.Context,
	jobID, expectedAgentID uuid.UUID,
	expectedAttempt int32,
	lines []LogLine,
) error {
	if len(lines) == 0 {
		return nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: bulk insert log lines for job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var rowAgent pgtype.UUID
	var rowAttempt int32
	if err := tx.QueryRow(ctx,
		`SELECT agent_id, attempt FROM job_runs WHERE id = $1 FOR UPDATE`, jobID,
	).Scan(&rowAgent, &rowAttempt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSnapshotStale
		}
		return fmt.Errorf("store: bulk insert log lines for job: lock: %w", err)
	}
	if fromPgUUID(rowAgent) != expectedAgentID || rowAttempt != expectedAttempt {
		return ErrSnapshotStale
	}
	if err := bulkInsertLogLinesQ(ctx, tx, lines); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: bulk insert log lines for job: commit: %w", err)
	}
	return nil
}

// bulkInsertLogLinesQ is the shared multi-VALUES build/exec used by
// both BulkInsertLogLines (no CAS) and BulkInsertLogLinesForJob
// (inside snapshot-CAS tx). `q` is anything implementing pgx's
// Exec — pool, tx, or conn.
type pgExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func bulkInsertLogLinesQ(ctx context.Context, q pgExecer, lines []LogLine) error {
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
	if _, err := q.Exec(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("store: bulk insert log lines: %w", err)
	}
	return nil
}

// CompleteJobInput captures the terminal payload coming off an agent's
// JobResult. Status must be "success" or "failed".
//
// ExpectedAgentID + ExpectedAttempt together form the load-bearing
// race guard: the SQL predicate uses
//
//	`agent_id IS NOT DISTINCT FROM @expected_agent_id
//	 AND attempt = @expected_attempt`
//
// so callers MUST set both correctly or the UPDATE will silently
// no-op (ok=false).
//
//   - Agent-driven result via handleJobResult: the caller looks up
//     the per-session assignment record (set at dispatch time —
//     see sessions.go RecordAssignment) and passes the recorded
//     (agent, attempt) pair. A stale result from a revoked session
//     for a job that has since been redispatched (same agent_id,
//     attempt bumped) won't match because the attempt mismatches.
//     This is the TOCTOU close-up: even if the session-revoked
//     check passed at handler entry, the SQL CAS still refuses to
//     complete the NEW attempt with the OLD exit code.
//
//   - Scheduler dispatch-time fail (failJobWithError): row is in
//     (queued, agent_id=NULL, attempt=0 unless reaper requeued).
//     Pass uuid.Nil + the current attempt fetched at observation
//     time (via DispatchableJob.Attempt).
type CompleteJobInput struct {
	JobRunID        uuid.UUID
	Status          string
	ExitCode        int32
	ErrorMsg        string
	ExpectedAgentID uuid.UUID
	ExpectedAttempt int32
	// Outputs is the structured k/v map the agent shipped in
	// JobResult.outputs (issue #10). Nil/empty is the common
	// case — the column defaults to '{}' so callers don't need
	// to think about non-output jobs. Persisted in the SAME
	// transaction as the status flip so downstream jobs gated on
	// this row's `needs:` always see the outputs as part of the
	// upstream's terminal state — no read-after-write race
	// against the dispatch path.
	Outputs map[string]string
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
	// Marshal outputs to JSONB. Nil/empty map → nil bytes → SQL
	// COALESCE falls back to '{}', keeping the legacy completion
	// shape one column wider but otherwise identical.
	var outputsJSON []byte
	if len(in.Outputs) > 0 {
		var err error
		outputsJSON, err = json.Marshal(in.Outputs)
		if err != nil {
			return JobCompletion{}, false, fmt.Errorf("store: complete job: marshal outputs: %w", err)
		}
	}
	row, err := q.CompleteJobRun(ctx, db.CompleteJobRunParams{
		ID:              pgUUID(in.JobRunID),
		Status:          in.Status,
		ExitCode:        &exitCode,
		Error:           nullableString(in.ErrorMsg),
		Outputs:         outputsJSON,
		ExpectedAgentID: pgUUIDNullable(in.ExpectedAgentID),
		ExpectedAttempt: in.ExpectedAttempt,
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

// FailJobWithReason marks a still-queued job as 'failed' (NOT
// 'skipped') with a human-readable reason on the `error` column,
// then cascades into stage/run terminal logic. Used by the
// scheduler's needs-satisfaction gate when an upstream is in a
// non-success terminal state — the downstream can never run, so
// we count it as a failure for aggregation purposes AND surface
// the chain in the error column so operators see why.
//
// Why `failed` and not `skipped`: GetStageProgress and
// GetRunUserStageOutcome only count `status='failed'` toward the
// run-failed aggregate. A 'skipped' downstream from needs-cascade
// would leak through as run = success despite a job that
// EXPECTED to run never running — confusing operator, fanout, and
// `on: success` notifications. Notification trigger skips
// (SkipJobRun) stay as 'skipped' because there the semantic is
// "by design, never going to run" — different from needs-cascade
// where the operator wrote `needs: [X]` expecting X to succeed.
//
// Returns ok=false (no error) when the row wasn't in 'queued' —
// another scheduler tick raced us OR a user manually canceled the
// run between our list and our fail. Caller logs at Debug and
// moves on, same shape as SkipNotificationJob's contract.
func (s *Store) FailJobWithReason(ctx context.Context, jobRunID uuid.UUID, reason string) (JobCompletion, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return JobCompletion{}, false, fmt.Errorf("store: fail job with reason: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.FailJobRunWithReason(ctx, db.FailJobRunWithReasonParams{
		ID:    pgUUID(jobRunID),
		Error: nullableString(reason),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JobCompletion{}, false, nil
		}
		return JobCompletion{}, false, fmt.Errorf("store: fail job with reason: %w", err)
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
		return JobCompletion{}, false, fmt.Errorf("store: fail job with reason: commit: %w", err)
	}
	return comp, true, nil
}

// ListJobStatusForRun returns the (name, matrix_key, status) tuple
// for every job_run in the given run. Used by the scheduler's
// dispatch tick to build a fast lookup map for needs-satisfaction
// checking — one round-trip, all the data needed to gate every
// candidate's `needs:` list. Stable order (name, matrix_key) so
// the per-name slices the scheduler builds are deterministic.
func (s *Store) ListJobStatusForRun(ctx context.Context, runID uuid.UUID) ([]JobStatusForRun, error) {
	rows, err := s.q.ListJobStatusForRun(ctx, pgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("store: list job status: %w", err)
	}
	out := make([]JobStatusForRun, 0, len(rows))
	for _, r := range rows {
		out = append(out, JobStatusForRun{
			Name:      r.Name,
			MatrixKey: stringValue(r.MatrixKey),
			Status:    r.Status,
		})
	}
	return out, nil
}

// JobStatusForRun is the lean shape returned by ListJobStatusForRun.
// Mirrors the scheduler's needs-check input row; kept in the store
// package so callers don't need to import db types.
type JobStatusForRun struct {
	Name      string
	MatrixKey string
	Status    string
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
