package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// DispatchableJob carries the queued-job shape the scheduler needs to build a
// JobAssignment without touching sqlc types.
type DispatchableJob struct {
	ID         uuid.UUID
	RunID      uuid.UUID
	StageRunID uuid.UUID
	Name       string
	MatrixKey  string
	Image      string
	Needs      []string
	// Attempt is the current retry counter on the row — needed by
	// dispatch-time fail paths so CompleteJob's snapshot CAS matches
	// the (NULL agent, current attempt) tuple instead of defaulting
	// to attempt=0 (which races with a reaper-requeue that bumped it).
	Attempt int32
}

// AssignedJob is the result of a successful AssignJob (row matched by
// optimistic predicate). Zero value signals another caller won the race.
type AssignedJob struct {
	ID      uuid.UUID
	RunID   uuid.UUID
	AgentID uuid.UUID
	Name    string
	// Attempt is the row's attempt counter AFTER AssignJob ran.
	// The scheduler stamps this on the target session via
	// RecordAssignment so the result handler can validate it as
	// the snapshot when CompleteJob fires — guards against a
	// stale revoked-session result completing a redispatched
	// attempt on the same agent UUID.
	Attempt int32
}

// RunForDispatch bundles the run row and its pipeline's definition snapshot,
// which is all the scheduler needs to materialize JobAssignments.
type RunForDispatch struct {
	ID         uuid.UUID
	PipelineID uuid.UUID
	ProjectID  uuid.UUID
	Counter    int64
	Status     string
	Revisions  json.RawMessage
	Definition json.RawMessage
	ConfigPath string
	// ProjectNotifications is the owning project's notifications
	// JSONB, pulled in the same round-trip as Definition so the
	// synth-notification dispatch path can fall back to it when
	// the pipeline didn't declare its own block.
	ProjectNotifications json.RawMessage
}

// OtherRunningRunForPipeline returns the run_id of an in-flight
// predecessor blocking this run from advancing past the serial-
// concurrency gate, or (uuid.Nil, false, nil) when no predecessor
// exists. The two-return-value (id, exists) shape lets callers do
// one query for both the gate decision AND the predecessor id they
// stamp onto runs.queue_reason for operator visibility (issue #4
// path #2).
//
// Replaces the prior boolean-returning OtherRunningRunExistsForPipeline
// — see queries/scheduler.sql for the migration rationale.
func (s *Store) OtherRunningRunForPipeline(ctx context.Context, pipelineID, runID uuid.UUID) (uuid.UUID, bool, error) {
	row, err := s.q.OtherRunningRunForPipeline(ctx,
		db.OtherRunningRunForPipelineParams{
			PipelineID: pgUUID(pipelineID),
			ID:         pgUUID(runID),
		})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("store: concurrency check: %w", err)
	}
	return fromPgUUID(row), true, nil
}

// SetRunQueueReason records why a queued run isn't advancing —
// today only the scheduler's serial-busy path uses this, but the
// column is plain TEXT so future producers (no-eligible-agent,
// approval-pending, ...) can extend the vocabulary. The store-side
// helper exists separately from ClearRunQueueReason so callers
// don't need to know about the `status='queued'` SQL guard.
func (s *Store) SetRunQueueReason(ctx context.Context, runID uuid.UUID, reason string) error {
	if err := s.q.SetRunQueueReason(ctx, db.SetRunQueueReasonParams{
		ID:          pgUUID(runID),
		QueueReason: &reason,
	}); err != nil {
		return fmt.Errorf("store: set queue_reason: %w", err)
	}
	return nil
}

// ClearRunQueueReason removes a previously-stamped reason. Idempotent
// — clearing an already-clear row is a cheap no-op via the
// IS NOT NULL guard in SQL. Called by the scheduler when the gate
// finally clears AND by the run-cancel path so a canceled-while-
// queued run doesn't carry a "waiting on #N" message in the list.
func (s *Store) ClearRunQueueReason(ctx context.Context, runID uuid.UUID) error {
	if err := s.q.ClearRunQueueReason(ctx, pgUUID(runID)); err != nil {
		return fmt.Errorf("store: clear queue_reason: %w", err)
	}
	return nil
}

// ListDispatchableJobs returns queued, unassigned jobs in the run's current
// active stage (lowest ordinal still holding queued/running work).
func (s *Store) ListDispatchableJobs(ctx context.Context, runID uuid.UUID) ([]DispatchableJob, error) {
	rows, err := s.q.ListDispatchableJobs(ctx, pgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("store: list dispatchable: %w", err)
	}
	out := make([]DispatchableJob, 0, len(rows))
	for _, r := range rows {
		out = append(out, DispatchableJob{
			ID:         fromPgUUID(r.ID),
			RunID:      fromPgUUID(r.RunID),
			StageRunID: fromPgUUID(r.StageRunID),
			Name:       r.Name,
			MatrixKey:  stringValue(r.MatrixKey),
			Image:      stringValue(r.Image),
			Needs:      append([]string(nil), r.Needs...),
			Attempt:    r.Attempt,
		})
	}
	return out, nil
}

// AssignJob flips a queued job to running atomically. Returns ok=false (and no
// error) when another scheduler tick or replica already claimed it.
func (s *Store) AssignJob(ctx context.Context, jobID, agentID uuid.UUID) (AssignedJob, bool, error) {
	row, err := s.q.AssignJob(ctx, db.AssignJobParams{
		ID:      pgUUID(jobID),
		AgentID: pgUUID(agentID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AssignedJob{}, false, nil
		}
		return AssignedJob{}, false, fmt.Errorf("store: assign job: %w", err)
	}
	return AssignedJob{
		ID:      fromPgUUID(row.ID),
		RunID:   fromPgUUID(row.RunID),
		AgentID: fromPgUUID(row.AgentID),
		Name:    row.Name,
		Attempt: row.Attempt,
	}, true, nil
}

// UnassignJob rolls back a successful AssignJob whose downstream
// Dispatch failed (busy queue, session vanished). Snapshot-CAS so
// the rollback only fires if the row is still in the exact state
// AssignJob just produced.
//
// Returns ok=true + runID when the rollback landed (caller fires
// NOTIFY so the scheduler retries the run on the next tick).
// Returns ok=false when the predicate doesn't match — meaning a
// reaper / register-fence / rerun already moved the row to a
// different state and our rollback would be incorrect.
//
// attempt is NOT bumped: a Dispatch that never reached an agent
// doesn't count as a failed attempt. The retry-cap logic in
// ReclaimJobForRetry is for AGENT-side failures.
func (s *Store) UnassignJob(ctx context.Context, jobID, agentID uuid.UUID, expectedAttempt int32) (uuid.UUID, bool, error) {
	row, err := s.q.UnassignJob(ctx, db.UnassignJobParams{
		ID:              pgUUID(jobID),
		AgentID:         pgUUID(agentID),
		ExpectedAttempt: expectedAttempt,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("store: unassign job: %w", err)
	}
	return fromPgUUID(row.RunID), true, nil
}

// MarkRunRunning promotes a queued run (idempotent: no-op if already running).
func (s *Store) MarkRunRunning(ctx context.Context, runID uuid.UUID) error {
	if err := s.q.MarkRunRunningIfQueued(ctx, pgUUID(runID)); err != nil {
		return fmt.Errorf("store: mark run running: %w", err)
	}
	return nil
}

// MarkStageRunning promotes a queued stage (idempotent).
func (s *Store) MarkStageRunning(ctx context.Context, stageRunID uuid.UUID) error {
	if err := s.q.MarkStageRunningIfQueued(ctx, pgUUID(stageRunID)); err != nil {
		return fmt.Errorf("store: mark stage running: %w", err)
	}
	return nil
}

// GetRunForDispatch loads the run + pipeline definition snapshot.
func (s *Store) GetRunForDispatch(ctx context.Context, runID uuid.UUID) (RunForDispatch, error) {
	row, err := s.q.GetRunForDispatch(ctx, pgUUID(runID))
	if err != nil {
		return RunForDispatch{}, fmt.Errorf("store: get run: %w", err)
	}
	return RunForDispatch{
		ID:                   fromPgUUID(row.ID),
		PipelineID:           fromPgUUID(row.PipelineID),
		ProjectID:            fromPgUUID(row.ProjectID),
		Counter:              row.Counter,
		Status:               row.Status,
		Revisions:            row.Revisions,
		Definition:           row.Definition,
		ConfigPath:           row.ConfigPath,
		ProjectNotifications: row.ProjectNotifications,
	}, nil
}

// ListQueuedRunIDs returns every run still in `queued` status, oldest first.
// Used by the scheduler's periodic tick as a LISTEN backstop.
func (s *Store) ListQueuedRunIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.q.ListQueuedRunIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list queued runs: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromPgUUID(r))
	}
	return out, nil
}

// ListPipelineMaterials returns every material row tied to a pipeline. Used
// when assembling MaterialCheckout entries on a JobAssignment.
func (s *Store) ListPipelineMaterials(ctx context.Context, pipelineID uuid.UUID) ([]Material, error) {
	rows, err := s.q.ListMaterialsByPipeline(ctx, pgUUID(pipelineID))
	if err != nil {
		return nil, fmt.Errorf("store: list materials: %w", err)
	}
	out := make([]Material, 0, len(rows))
	for _, r := range rows {
		out = append(out, Material{
			ID:          fromPgUUID(r.ID),
			PipelineID:  fromPgUUID(r.PipelineID),
			Type:        r.Type,
			Config:      r.Config,
			Fingerprint: r.Fingerprint,
			AutoUpdate:  r.AutoUpdate,
			CreatedAt:   r.CreatedAt.Time,
		})
	}
	return out, nil
}
