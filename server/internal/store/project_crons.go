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
)

// ErrProjectCronNotFound is returned by GetProjectCron when no row
// matches the requested id. Lets API handlers distinguish 404 from
// a genuine internal error.
var ErrProjectCronNotFound = errors.New("store: project_cron not found")

// ProjectCron is the decoded domain shape of one project-level
// schedule. PipelineIDs empty = "every pipeline in the project
// at fire time" (dynamic membership, see ticker).
type ProjectCron struct {
	ID           uuid.UUID
	ProjectID    uuid.UUID
	Name         string
	Expression   string
	PipelineIDs  []uuid.UUID
	Enabled      bool
	LastFiredAt  *time.Time
	CreatedBy    *uuid.UUID
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ProjectCronInput is the write shape for Insert + Update. CreatedBy
// is ignored on update (the store doesn't reassign ownership on
// edit — UI deliberately has no such action).
type ProjectCronInput struct {
	ProjectID   uuid.UUID
	Name        string
	Expression  string
	PipelineIDs []uuid.UUID
	Enabled     bool
	CreatedBy   *uuid.UUID
}

// ListProjectCrons returns all schedules bound to a project,
// newest-first so the UI's list reads "latest at top".
func (s *Store) ListProjectCrons(ctx context.Context, projectID uuid.UUID) ([]ProjectCron, error) {
	rows, err := s.q.ListProjectCronsByProject(ctx, pgUUID(projectID))
	if err != nil {
		return nil, fmt.Errorf("store: list project_crons: %w", err)
	}
	out := make([]ProjectCron, 0, len(rows))
	for _, r := range rows {
		out = append(out, ProjectCron{
			ID:          fromPgUUID(r.ID),
			ProjectID:   fromPgUUID(r.ProjectID),
			Name:        r.Name,
			Expression:  r.Expression,
			PipelineIDs: fromPgUUIDs(r.PipelineIds),
			Enabled:     r.Enabled,
			LastFiredAt: pgTimePtr(r.LastFiredAt),
			CreatedBy:   pgUUIDPtr(r.CreatedBy),
			CreatedAt:   r.CreatedAt.Time,
			UpdatedAt:   r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// EnabledProjectCron is the slim shape the ticker consumes:
// only what it needs to evaluate + fire.
type EnabledProjectCron struct {
	ID          uuid.UUID
	ProjectID   uuid.UUID
	Name        string
	Expression  string
	PipelineIDs []uuid.UUID
	LastFiredAt *time.Time
}

// ListEnabledProjectCrons returns every enabled schedule system-
// wide, ordered by id for deterministic tick ordering.
func (s *Store) ListEnabledProjectCrons(ctx context.Context) ([]EnabledProjectCron, error) {
	rows, err := s.q.ListEnabledProjectCrons(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled project_crons: %w", err)
	}
	out := make([]EnabledProjectCron, 0, len(rows))
	for _, r := range rows {
		out = append(out, EnabledProjectCron{
			ID:          fromPgUUID(r.ID),
			ProjectID:   fromPgUUID(r.ProjectID),
			Name:        r.Name,
			Expression:  r.Expression,
			PipelineIDs: fromPgUUIDs(r.PipelineIds),
			LastFiredAt: pgTimePtr(r.LastFiredAt),
		})
	}
	return out, nil
}

// GetProjectCron returns one schedule by id. Returns
// ErrProjectCronNotFound so handlers can 404 cleanly.
func (s *Store) GetProjectCron(ctx context.Context, id uuid.UUID) (ProjectCron, error) {
	row, err := s.q.GetProjectCron(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProjectCron{}, ErrProjectCronNotFound
		}
		return ProjectCron{}, fmt.Errorf("store: get project_cron: %w", err)
	}
	return ProjectCron{
		ID:          fromPgUUID(row.ID),
		ProjectID:   fromPgUUID(row.ProjectID),
		Name:        row.Name,
		Expression:  row.Expression,
		PipelineIDs: fromPgUUIDs(row.PipelineIds),
		Enabled:     row.Enabled,
		LastFiredAt: pgTimePtr(row.LastFiredAt),
		CreatedBy:   pgUUIDPtr(row.CreatedBy),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}, nil
}

// InsertProjectCron creates a new schedule. Returns the persisted
// row (including the generated id + timestamps) so callers can
// echo back to the UI without an extra read.
func (s *Store) InsertProjectCron(ctx context.Context, in ProjectCronInput) (ProjectCron, error) {
	params := db.InsertProjectCronParams{
		ProjectID:   pgUUID(in.ProjectID),
		Name:        in.Name,
		Expression:  in.Expression,
		PipelineIds: toPgUUIDs(in.PipelineIDs),
		Enabled:     in.Enabled,
		CreatedBy:   pgUUIDFromPtr(in.CreatedBy),
	}
	row, err := s.q.InsertProjectCron(ctx, params)
	if err != nil {
		return ProjectCron{}, fmt.Errorf("store: insert project_cron: %w", err)
	}
	return ProjectCron{
		ID:          fromPgUUID(row.ID),
		ProjectID:   fromPgUUID(row.ProjectID),
		Name:        row.Name,
		Expression:  row.Expression,
		PipelineIDs: fromPgUUIDs(row.PipelineIds),
		Enabled:     row.Enabled,
		LastFiredAt: pgTimePtr(row.LastFiredAt),
		CreatedBy:   pgUUIDPtr(row.CreatedBy),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}, nil
}

// UpdateProjectCron overwrites name/expression/pipelines/enabled.
// last_fired_at + created_by stay untouched — the ticker owns
// the former, UI deliberately doesn't reassign the latter.
func (s *Store) UpdateProjectCron(ctx context.Context, id uuid.UUID, in ProjectCronInput) error {
	err := s.q.UpdateProjectCron(ctx, db.UpdateProjectCronParams{
		ID:          pgUUID(id),
		Name:        in.Name,
		Expression:  in.Expression,
		PipelineIds: toPgUUIDs(in.PipelineIDs),
		Enabled:     in.Enabled,
	})
	if err != nil {
		return fmt.Errorf("store: update project_cron: %w", err)
	}
	return nil
}

// DeleteProjectCron removes one schedule. Idempotent at the SQL
// layer — deleting a non-existent id is a no-op, matching the
// HTTP delete contract (204 regardless).
func (s *Store) DeleteProjectCron(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteProjectCron(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("store: delete project_cron: %w", err)
	}
	return nil
}

// MarkProjectCronFired stamps last_fired_at after a successful
// fire cycle. Non-fatal from the ticker's perspective: a crash
// between fire + mark leaves the schedule replay-safe because
// the expression parser uses last_fired_at as its baseline —
// worst case one extra fire on next tick, which idempotent
// downstream constructs (modification unique key) absorb.
func (s *Store) MarkProjectCronFired(ctx context.Context, id uuid.UUID, firedAt time.Time) error {
	if err := s.q.MarkProjectCronFired(ctx, db.MarkProjectCronFiredParams{
		ID:          pgUUID(id),
		LastFiredAt: pgtype.Timestamptz{Time: firedAt, Valid: true},
	}); err != nil {
		return fmt.Errorf("store: mark project_cron fired: %w", err)
	}
	return nil
}

// ListPipelineIDsByProject returns every pipeline id in a project.
// Used by the ticker when a schedule has empty pipeline_ids
// ("fire all") and by the run-all handler. Empty slice on an
// empty / detached project.
func (s *Store) ListPipelineIDsByProject(ctx context.Context, projectID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.ListPipelinesByProject(ctx, pgUUID(projectID))
	if err != nil {
		return nil, fmt.Errorf("store: list pipeline ids by project: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromPgUUID(r.ID))
	}
	return out, nil
}

// --- small helpers for uuid array + pointer marshalling ---

func toPgUUIDs(ids []uuid.UUID) []pgtype.UUID {
	if len(ids) == 0 {
		return []pgtype.UUID{}
	}
	out := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		out[i] = pgUUID(id)
	}
	return out
}

func fromPgUUIDs(in []pgtype.UUID) []uuid.UUID {
	if len(in) == 0 {
		return nil
	}
	out := make([]uuid.UUID, 0, len(in))
	for _, x := range in {
		if x.Valid {
			out = append(out, fromPgUUID(x))
		}
	}
	return out
}

func pgUUIDFromPtr(p *uuid.UUID) pgtype.UUID {
	if p == nil {
		return pgtype.UUID{}
	}
	return pgUUID(*p)
}

func pgUUIDPtr(x pgtype.UUID) *uuid.UUID {
	if !x.Valid {
		return nil
	}
	u := fromPgUUID(x)
	return &u
}
