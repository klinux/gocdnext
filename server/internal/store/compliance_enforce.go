package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/compliance"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// policiesForProject resolves the enabled compliance policies that apply to a
// project (global or framework-targeted) and compiles them into merge-ready
// form. This is the apply-time hot path; it runs on the same tx as the project
// upsert so a racing policy change can't make the snapshot disagree.
func policiesForProject(ctx context.Context, q *db.Queries, projectID pgtype.UUID) ([]compliance.Policy, error) {
	rows, err := q.ResolvePoliciesForProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: resolve policies: %w", err)
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

// recomputeProjectEffective re-merges every pipeline of a project from its
// stored raw (pre-policy) definition with the project's current policies,
// refreshing the effective definition that materialisation + dispatch read.
// Called after any change to which policies apply (policy CRUD, framework
// delete, project-framework assignment).
func recomputeProjectEffective(ctx context.Context, q *db.Queries, projectID pgtype.UUID) error {
	policies, err := policiesForProject(ctx, q, projectID)
	if err != nil {
		return err
	}
	raws, err := q.ListPipelineRawByProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("store: list raw pipelines: %w", err)
	}
	for _, row := range raws {
		var raw domain.Pipeline
		if err := json.Unmarshal(row.DefinitionRaw, &raw); err != nil {
			return fmt.Errorf("store: decode raw pipeline: %w", err)
		}
		eff := compliance.ApplyPolicies(raw, policies)
		effBytes, err := marshalPipelineDefinition(&eff)
		if err != nil {
			return fmt.Errorf("store: marshal effective pipeline: %w", err)
		}
		if _, err := q.UpdatePipelineEffectiveDefinition(ctx, db.UpdatePipelineEffectiveDefinitionParams{
			ID:         row.ID,
			Definition: effBytes,
		}); err != nil {
			return fmt.Errorf("store: update effective pipeline: %w", err)
		}
	}

	// Same reconciliation as the apply path: create/drop the synthetic
	// pipeline and refresh non-suppressible triggers as governance changes
	// (e.g. a framework just assigned to a pipeline-less project).
	return reconcileGovernance(ctx, q, projectID, ".gocdnext", policies)
}

// recomputeProjects runs recomputeProjectEffective for a set of projects. It
// runs in the same tx as the triggering mutation (under the exclusive compliance
// advisory lock) so the change is atomic and the apply-vs-mutation race can't
// persist a stale effective definition.
//
// SCALING (known Phase-1 tradeoff): for an `applies_to_all` change the set is
// every project, so this is a single large transaction holding the exclusive
// lock for its duration — it blocks concurrent compliance writes and project
// applies (NOT run dispatch, which never takes the lock). The correct fix for
// large fleets is a policy generation counter + a background batched worker
// (recompute per project in its own short tx, idempotent under the generation),
// tracked as a follow-up. Until then, correctness is preferred over throughput.
func recomputeProjects(ctx context.Context, q *db.Queries, ids []pgtype.UUID) error {
	for _, id := range ids {
		if err := recomputeProjectEffective(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

// affectedProjectIDs returns the projects whose effective definition a policy
// change touches: all projects when the policy is global, otherwise those
// carrying any of the given frameworks.
func affectedProjectIDs(ctx context.Context, q *db.Queries, appliesToAll bool, frameworkIDs []pgtype.UUID) ([]pgtype.UUID, error) {
	if appliesToAll {
		ids, err := q.ListAllProjectIDs(ctx)
		if err != nil {
			return nil, fmt.Errorf("store: list all projects: %w", err)
		}
		return ids, nil
	}
	if len(frameworkIDs) == 0 {
		return nil, nil
	}
	ids, err := q.ListProjectIDsByFrameworks(ctx, frameworkIDs)
	if err != nil {
		return nil, fmt.Errorf("store: list projects by frameworks: %w", err)
	}
	return ids, nil
}
