package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ServiceRunInput is the agent-shipped tuple the grpcsrv layer
// builds from a ServiceLifecycle proto. Defined here so callers
// don't have to import sqlc-generated types — keeps the boundary
// between `store` and `grpcsrv` clean.
type ServiceRunInput struct {
	RunID   uuid.UUID
	Name    string
	Image   string
	PodName string
	Status  string // starting|ready|stopped|failed
	At      time.Time
	Error   string
}

// UpsertServiceRun persists a single lifecycle event into
// `service_runs`. Idempotent on (run_id, name). The query itself
// is COALESCE-aware so re-issuing `ready` doesn't reset the
// first-observed started_at, and an out-of-order `starting`
// arriving after `ready` doesn't clobber the ready timestamp.
//
// Returns the post-upsert row so the caller has the canonical
// view (useful for tests; production callers ignore it today).
func (s *Store) UpsertServiceRun(ctx context.Context, in ServiceRunInput) (db.ServiceRun, error) {
	row, err := s.q.UpsertServiceRun(ctx, db.UpsertServiceRunParams{
		RunID:   pgtype.UUID{Bytes: in.RunID, Valid: true},
		Name:    in.Name,
		Image:   in.Image,
		PodName: in.PodName,
		Status:  in.Status,
		At:      pgtype.Timestamptz{Time: in.At, Valid: true},
		Error:   in.Error,
	})
	if err != nil {
		return db.ServiceRun{}, fmt.Errorf("upsert service run (run=%s name=%s status=%s): %w",
			in.RunID, in.Name, in.Status, err)
	}
	return row, nil
}

// ListServiceRunsByRunID powers GET /api/runs/{id}/services.
// Stable alphabetical order by name — see the SQL comment for
// why declaration order isn't preserved here.
func (s *Store) ListServiceRunsByRunID(ctx context.Context, runID uuid.UUID) ([]db.ServiceRun, error) {
	rows, err := s.q.ListServiceRunsByRunID(ctx, pgtype.UUID{Bytes: runID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("list service runs (run=%s): %w", runID, err)
	}
	return rows, nil
}
