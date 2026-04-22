package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// CronMaterialRow is the decoded view of a cron material plus its
// last-fire bookkeeping. The ticker walks a slice of these on
// every tick and decides whether to dispatch a run from the
// pipeline this material belongs to.
type CronMaterialRow struct {
	MaterialID  uuid.UUID
	PipelineID  uuid.UUID
	ProjectID   uuid.UUID
	Expression  string
	LastFiredAt *time.Time
}

// ListCronMaterials returns every cron material in the system
// with its current fire-state. Empty slice (not error) when no
// pipeline declares a cron trigger.
func (s *Store) ListCronMaterials(ctx context.Context) ([]CronMaterialRow, error) {
	rows, err := s.q.ListCronMaterials(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list cron materials: %w", err)
	}
	out := make([]CronMaterialRow, 0, len(rows))
	for _, r := range rows {
		var cfg domain.CronMaterial
		if err := json.Unmarshal(r.Config, &cfg); err != nil {
			return nil, fmt.Errorf("store: decode cron material %s: %w",
				fromPgUUID(r.ID), err)
		}
		out = append(out, CronMaterialRow{
			MaterialID:  fromPgUUID(r.ID),
			PipelineID:  fromPgUUID(r.PipelineID),
			ProjectID:   fromPgUUID(r.ProjectID),
			Expression:  cfg.Expression,
			LastFiredAt: pgTimePtr(r.LastFiredAt),
		})
	}
	return out, nil
}

// MarkCronFired stamps `firedAt` as the last time this cron
// material triggered a run. The ticker calls this right after
// CreateRunFromModification so a crash before the update still
// leaves the system consistent (worst case: a fire is replayed
// after restart; the next-fire calc will skip it via the
// idempotency check inside CreateRunFromModification's unique
// modification key).
func (s *Store) MarkCronFired(ctx context.Context, materialID uuid.UUID, firedAt time.Time) error {
	if err := s.q.UpsertCronFired(ctx, db.UpsertCronFiredParams{
		MaterialID:  pgUUID(materialID),
		LastFiredAt: pgtype.Timestamptz{Time: firedAt, Valid: true},
	}); err != nil {
		return fmt.Errorf("store: mark cron fired: %w", err)
	}
	return nil
}
