package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/compliance"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// EffectivePipelineView is one pipeline's pre-policy (raw) and post-merge
// (effective) definition, for the admin "preview effective pipeline" panel.
// SystemManaged marks the server-owned synthetic `_compliance` pipeline.
type EffectivePipelineView struct {
	Name          string
	SystemManaged bool
	Raw           domain.Pipeline
	Effective     domain.Pipeline
}

// PreviewEffectivePipelines returns, for every pipeline of a project, its raw
// (pre-policy) and effective (post-merge) definition. It is READ-ONLY — nothing
// is ever persisted, so it takes no advisory lock.
//
// whatIfFrameworks selects the mode:
//   - nil      → READ the stored effective definition (what runs today); a plain
//     two-column read, no merge recompute.
//   - non-nil  → WHAT-IF: recompute the effective definition by applying the
//     policies that WOULD govern the project under that hypothetical framework
//     set (an empty, non-nil slice means "no frameworks" → only global
//     applies-to-all policies). The project's stored assignment is untouched.
//
// Returns ErrProjectNotFound for an unknown slug.
func (s *Store) PreviewEffectivePipelines(ctx context.Context, slug string, whatIfFrameworks *[]string) ([]EffectivePipelineView, error) {
	proj, err := s.q.GetProjectBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrProjectNotFound
		}
		return nil, fmt.Errorf("store: preview: project by slug: %w", err)
	}
	rows, err := s.q.ListPipelinesForPreview(ctx, proj.ID)
	if err != nil {
		return nil, fmt.Errorf("store: preview: list pipelines: %w", err)
	}
	if whatIfFrameworks == nil {
		return previewStored(rows)
	}
	return s.previewWhatIf(ctx, proj.ID, rows, *whatIfFrameworks)
}

// previewStored decodes the already-materialised raw + effective definitions.
func previewStored(rows []db.ListPipelinesForPreviewRow) ([]EffectivePipelineView, error) {
	out := make([]EffectivePipelineView, 0, len(rows))
	for _, r := range rows {
		raw, err := decodePipelineDef(r.DefinitionRaw, "raw "+r.Name)
		if err != nil {
			return nil, err
		}
		eff, err := decodePipelineDef(r.Definition, "effective "+r.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, EffectivePipelineView{
			Name: r.Name, SystemManaged: r.SystemManaged, Raw: raw, Effective: eff,
		})
	}
	return out, nil
}

// previewWhatIf recomputes the effective definition for a hypothetical framework
// set without touching stored state. Repo pipelines are re-merged from their raw
// definition; the server-owned synthetic pipeline is reconstructed (not read)
// because it would only exist under the hypothetical governance — mirroring
// reconcileGovernance so the preview matches what an assignment would produce.
func (s *Store) previewWhatIf(ctx context.Context, projectID pgtype.UUID, rows []db.ListPipelinesForPreviewRow, frameworkIDs []string) ([]EffectivePipelineView, error) {
	fwIDs, err := parseUUIDs(frameworkIDs)
	if err != nil {
		return nil, fmt.Errorf("store: preview what-if: %w", err)
	}
	policies, err := policiesByFrameworks(ctx, s.q, fwIDs)
	if err != nil {
		return nil, err
	}
	out := make([]EffectivePipelineView, 0, len(rows)+1)
	repoCount := 0
	for _, r := range rows {
		if r.SystemManaged {
			// Recomputed below from the hypothetical governance, not read.
			continue
		}
		repoCount++
		raw, err := decodePipelineDef(r.DefinitionRaw, "raw "+r.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, EffectivePipelineView{
			Name: r.Name, Raw: raw, Effective: compliance.ApplyPolicies(raw, policies),
		})
	}
	// A governed project with no pipeline of its own runs policies via the
	// synthetic pipeline. Only show it when the project actually has an SCM
	// binding (push-triggered enforcement requires one — see reconcileGovernance).
	if repoCount == 0 && len(policies) > 0 {
		scmURL, branch, hasSCM, err := scmForProject(ctx, s.q, projectID)
		if err != nil {
			return nil, err
		}
		if hasSCM {
			raw := compliance.SyntheticPipeline(scmURL, branch)
			out = append(out, EffectivePipelineView{
				Name: compliance.PipelineName, SystemManaged: true,
				Raw: raw, Effective: compliance.ApplyPolicies(raw, policies),
			})
		}
	}
	return out, nil
}

// policiesByFrameworks compiles the enabled policies that apply to a hypothetical
// framework set into merge-ready form. Mirrors policiesForProject, keyed off the
// what-if framework set rather than the project's stored assignment.
func policiesByFrameworks(ctx context.Context, q *db.Queries, frameworkIDs []pgtype.UUID) ([]compliance.Policy, error) {
	rows, err := q.ResolvePoliciesByFrameworks(ctx, frameworkIDs)
	if err != nil {
		return nil, fmt.Errorf("store: resolve policies by frameworks: %w", err)
	}
	out := make([]compliance.Policy, 0, len(rows))
	for _, r := range rows {
		var cfg domain.Pipeline
		if err := json.Unmarshal(r.Config, &cfg); err != nil {
			return nil, fmt.Errorf("store: decode policy %q config: %w", r.Name, err)
		}
		out = append(out, compliance.Policy{
			Name:           r.Name,
			Mode:           r.Mode,
			Priority:       int(r.Priority),
			PositionBefore: r.PositionBefore,
			PositionAfter:  r.PositionAfter,
			Pipeline:       cfg,
		})
	}
	return out, nil
}

// decodePipelineDef unmarshals a stored pipeline definition. An empty blob (no
// definition persisted) decodes to a zero Pipeline rather than an error.
func decodePipelineDef(b []byte, label string) (domain.Pipeline, error) {
	var p domain.Pipeline
	if len(b) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("store: preview: decode %s: %w", label, err)
	}
	return p, nil
}
