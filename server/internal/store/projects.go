package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// ApplyProjectInput is the declarative payload from `gocdnext apply`: a project
// and the full list of pipelines that should exist under it. Pipelines not in
// this list are removed; materials not in a pipeline's list are removed.
type ApplyProjectInput struct {
	Slug        string
	Name        string
	Description string
	ConfigRepo  string
	Pipelines   []*domain.Pipeline
}

type PipelineApplyStatus struct {
	Name             string
	PipelineID       uuid.UUID
	Created          bool
	MaterialsAdded   int
	MaterialsRemoved int
}

type ApplyProjectResult struct {
	ProjectID        uuid.UUID
	ProjectCreated   bool
	Pipelines        []PipelineApplyStatus
	PipelinesRemoved []string
}

// ApplyProject upserts the project and synchronizes its pipelines and materials
// to match the input. The whole operation runs inside one transaction: either
// every row reflects the input or nothing changes.
func (s *Store) ApplyProject(ctx context.Context, in ApplyProjectInput) (ApplyProjectResult, error) {
	if in.Slug == "" {
		return ApplyProjectResult{}, fmt.Errorf("store: apply project: slug is required")
	}
	if in.Name == "" {
		in.Name = in.Slug
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ApplyProjectResult{}, fmt.Errorf("store: apply project: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	proj, err := q.UpsertProject(ctx, db.UpsertProjectParams{
		Slug:        in.Slug,
		Name:        in.Name,
		Description: nullableString(in.Description),
	})
	if err != nil {
		return ApplyProjectResult{}, fmt.Errorf("store: upsert project: %w", err)
	}

	result := ApplyProjectResult{
		ProjectID:      fromPgUUID(proj.ID),
		ProjectCreated: proj.Created,
	}

	wanted := make(map[string]*domain.Pipeline, len(in.Pipelines))
	for _, p := range in.Pipelines {
		if p.Name == "" {
			return ApplyProjectResult{}, fmt.Errorf("store: pipeline without name")
		}
		if _, dup := wanted[p.Name]; dup {
			return ApplyProjectResult{}, fmt.Errorf("store: pipeline %q listed twice", p.Name)
		}
		wanted[p.Name] = p
	}

	existing, err := q.ListPipelinesByProject(ctx, proj.ID)
	if err != nil {
		return ApplyProjectResult{}, fmt.Errorf("store: list pipelines: %w", err)
	}
	for _, row := range existing {
		if _, keep := wanted[row.Name]; keep {
			continue
		}
		if err := q.DeletePipeline(ctx, row.ID); err != nil {
			return ApplyProjectResult{}, fmt.Errorf("store: delete pipeline %s: %w", row.Name, err)
		}
		result.PipelinesRemoved = append(result.PipelinesRemoved, row.Name)
	}

	for _, p := range in.Pipelines {
		status, err := applyPipeline(ctx, q, proj.ID, p, in.ConfigRepo)
		if err != nil {
			return ApplyProjectResult{}, err
		}
		result.Pipelines = append(result.Pipelines, status)
	}

	if err := tx.Commit(ctx); err != nil {
		return ApplyProjectResult{}, fmt.Errorf("store: apply project: commit: %w", err)
	}
	return result, nil
}

func applyPipeline(ctx context.Context, q *db.Queries, projectID pgtype.UUID, p *domain.Pipeline, configRepo string) (PipelineApplyStatus, error) {
	definition, err := marshalPipelineDefinition(p)
	if err != nil {
		return PipelineApplyStatus{}, fmt.Errorf("store: marshal pipeline %s: %w", p.Name, err)
	}

	row, err := q.UpsertPipeline(ctx, db.UpsertPipelineParams{
		ProjectID:  projectID,
		Name:       p.Name,
		Definition: definition,
		ConfigRepo: nullableString(configRepo),
		ConfigPath: ".gocdnext",
	})
	if err != nil {
		return PipelineApplyStatus{}, fmt.Errorf("store: upsert pipeline %s: %w", p.Name, err)
	}

	status := PipelineApplyStatus{
		Name:       row.Name,
		PipelineID: fromPgUUID(row.ID),
		Created:    row.Created,
	}

	existing, err := q.ListMaterialsByPipeline(ctx, row.ID)
	if err != nil {
		return PipelineApplyStatus{}, fmt.Errorf("store: list materials %s: %w", p.Name, err)
	}
	existingByFP := make(map[string]db.Material, len(existing))
	for _, m := range existing {
		existingByFP[m.Fingerprint] = m
	}

	wantedFPs := make(map[string]struct{}, len(p.Materials))
	for _, m := range p.Materials {
		wantedFPs[m.Fingerprint] = struct{}{}
		cfg, err := marshalMaterialConfig(m)
		if err != nil {
			return PipelineApplyStatus{}, fmt.Errorf("store: marshal material %s/%s: %w", p.Name, m.Fingerprint, err)
		}
		res, err := q.UpsertMaterial(ctx, db.UpsertMaterialParams{
			PipelineID:  row.ID,
			Type:        string(m.Type),
			Config:      cfg,
			Fingerprint: m.Fingerprint,
			AutoUpdate:  m.AutoUpdate,
		})
		if err != nil {
			return PipelineApplyStatus{}, fmt.Errorf("store: upsert material %s: %w", m.Fingerprint, err)
		}
		if res.Created {
			status.MaterialsAdded++
		}
	}

	for fp, m := range existingByFP {
		if _, keep := wantedFPs[fp]; keep {
			continue
		}
		if err := q.DeleteMaterial(ctx, m.ID); err != nil {
			return PipelineApplyStatus{}, fmt.Errorf("store: delete material %s: %w", fp, err)
		}
		status.MaterialsRemoved++
	}

	return status, nil
}

func marshalPipelineDefinition(p *domain.Pipeline) ([]byte, error) {
	clone := *p
	clone.ID = ""
	clone.ProjectID = ""
	for i := range clone.Materials {
		clone.Materials[i].ID = ""
	}
	return json.Marshal(clone)
}

func marshalMaterialConfig(m domain.Material) ([]byte, error) {
	switch m.Type {
	case domain.MaterialGit:
		return json.Marshal(m.Git)
	case domain.MaterialUpstream:
		return json.Marshal(m.Upstream)
	case domain.MaterialCron:
		return json.Marshal(m.Cron)
	case domain.MaterialManual:
		return []byte(`{}`), nil
	default:
		return nil, fmt.Errorf("unknown material type %q", m.Type)
	}
}
