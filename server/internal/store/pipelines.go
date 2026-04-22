package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// ErrPipelineNotFound signals the pipeline ID doesn't resolve to a
// row. Handlers should translate it to 404 (not 500).
var ErrPipelineNotFound = errors.New("pipeline not found")

// GetPipelineByID loads the pipeline's full definition JSONB and
// unmarshals it into a domain.Pipeline. Used by read paths that
// need the canonical structure (YAML tab, future "export" flow),
// as opposed to the thin PipelineSummary driven by read models.
func (s *Store) GetPipelineByID(ctx context.Context, id uuid.UUID) (*domain.Pipeline, error) {
	row, err := s.q.GetPipelineDefinition(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPipelineNotFound
		}
		return nil, fmt.Errorf("store: get pipeline %s: %w", id, err)
	}
	var p domain.Pipeline
	if err := json.Unmarshal(row.Definition, &p); err != nil {
		return nil, fmt.Errorf("store: decode pipeline %s: %w", id, err)
	}
	p.ID = fromPgUUID(row.ID).String()
	p.ProjectID = fromPgUUID(row.ProjectID).String()
	p.Name = row.Name
	return &p, nil
}
