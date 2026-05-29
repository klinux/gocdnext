package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

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

// FindMaterialsByFingerprint returns every material row that hashes
// to the same (url, branch) fingerprint. Materials are uniqued on
// (pipeline_id, fingerprint), so N pipelines that watch the same
// (repo, branch) legitimately share a hash — the caller (webhook
// handlers, today) must fan out one run per row.
//
// Returns an EMPTY slice (not nil + error) for no matches; callers
// distinguish "no pipeline material matched" from a hard DB error
// by checking len(materials) instead of errors.Is(ErrMaterialNotFound).
// Hard DB errors are wrapped and returned verbatim.
func (s *Store) FindMaterialsByFingerprint(ctx context.Context, fingerprint string) ([]Material, error) {
	rows, err := s.q.FindMaterialsByFingerprint(ctx, fingerprint)
	if err != nil {
		return nil, fmt.Errorf("store: find materials: %w", err)
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
