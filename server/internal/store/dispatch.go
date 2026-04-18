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
}

// AssignedJob is the result of a successful AssignJob (row matched by
// optimistic predicate). Zero value signals another caller won the race.
type AssignedJob struct {
	ID      uuid.UUID
	RunID   uuid.UUID
	AgentID uuid.UUID
	Name    string
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
	}, true, nil
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
		ID:         fromPgUUID(row.ID),
		PipelineID: fromPgUUID(row.PipelineID),
		ProjectID:  fromPgUUID(row.ProjectID),
		Counter:    row.Counter,
		Status:     row.Status,
		Revisions:  row.Revisions,
		Definition: row.Definition,
		ConfigPath: row.ConfigPath,
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
