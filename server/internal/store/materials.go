package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/domain"
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

// GitMaterialRow pairs a material's database id with the parsed git
// config — enough for the trigger-seed path to decide which repo/branch
// to ask for HEAD and which material_id to attach the new modification
// to. Non-git materials are excluded by the query wrapper.
type GitMaterialRow struct {
	ID     uuid.UUID
	Config domain.GitMaterial
}

// ListGitMaterialsForPipeline returns every git material attached to a
// pipeline with its config decoded. Non-git materials (upstream, cron,
// manual) are filtered out here so the trigger-seed path can iterate
// without re-checking the Type discriminator on every row. Returns an
// empty slice (not an error) when the pipeline has zero git materials
// — the caller decides what to do with a cron-only or upstream-only
// pipeline.
func (s *Store) ListGitMaterialsForPipeline(ctx context.Context, pipelineID uuid.UUID) ([]GitMaterialRow, error) {
	rows, err := s.q.ListMaterialsByPipeline(ctx, pgUUID(pipelineID))
	if err != nil {
		return nil, fmt.Errorf("store: list materials: %w", err)
	}
	out := make([]GitMaterialRow, 0, len(rows))
	for _, r := range rows {
		if r.Type != string(domain.MaterialGit) {
			continue
		}
		var cfg domain.GitMaterial
		if err := json.Unmarshal(r.Config, &cfg); err != nil {
			return nil, fmt.Errorf("store: decode git material %s: %w", fromPgUUID(r.ID), err)
		}
		out = append(out, GitMaterialRow{
			ID:     fromPgUUID(r.ID),
			Config: cfg,
		})
	}
	return out, nil
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
