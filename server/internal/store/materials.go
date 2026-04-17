package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Material mirrors the columns we need from the materials row. JSONB config
// is surfaced as raw message so each material type can decode it as it likes.
type Material struct {
	ID          uuid.UUID
	PipelineID  uuid.UUID
	Type        string
	Config      json.RawMessage
	Fingerprint string
	AutoUpdate  bool
	CreatedAt   time.Time
}

// FindMaterialByFingerprint returns ErrMaterialNotFound if no row matches.
// Other errors (DB down, driver issue) are wrapped and returned verbatim.
func (s *Store) FindMaterialByFingerprint(ctx context.Context, fingerprint string) (Material, error) {
	row, err := s.q.FindMaterialByFingerprint(ctx, fingerprint)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Material{}, ErrMaterialNotFound
		}
		return Material{}, fmt.Errorf("store: find material: %w", err)
	}
	return Material{
		ID:          fromPgUUID(row.ID),
		PipelineID:  fromPgUUID(row.PipelineID),
		Type:        row.Type,
		Config:      row.Config,
		Fingerprint: row.Fingerprint,
		AutoUpdate:  row.AutoUpdate,
		CreatedAt:   row.CreatedAt.Time,
	}, nil
}
