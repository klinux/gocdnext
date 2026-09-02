package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedTwoProjectsWithDeploy applies 2 projects (payments + storefront),
// each with a `build` and `deploy` pipeline, and inserts one queued run
// per pipeline (4 runs total). Returns the pipeline IDs keyed by
// "<slug>/<pipeline>" so tests can pin runs to specific pipelines.
//
// Shape mirrors the real /runs use case that motivated `pipeline_filter`:
// "show me every failed deploy across projects" — pipeline names repeat
// across projects, so filtering by name is the right axis.
func seedTwoProjectsWithDeploy(t *testing.T, s *store.Store, ctx context.Context) map[string]uuid.UUID {
	t.Helper()
	pipelines := make(map[string]uuid.UUID)

	for _, slug := range []string{"payments", "storefront"} {
		res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
			Slug: slug, Name: slug,
			Pipelines: []*domain.Pipeline{
				{
					Name: "build", Stages: []string{"check"},
					Jobs: []domain.Job{{Name: "test", Stage: "check"}},
				},
				{
					Name: "deploy", Stages: []string{"ship"},
					Jobs: []domain.Job{{Name: "roll", Stage: "ship"}},
				},
			},
		})
		if err != nil {
			t.Fatalf("apply %s: %v", slug, err)
		}
		for _, p := range res.Pipelines {
			pipelines[slug+"/"+p.Name] = uuid.UUID(p.PipelineID)
		}
	}
	return pipelines
}

// insertRun inserts a run for the given pipeline. Minimal shape —
// filter queries only exercise pipeline_id/status/cause/project. Takes
// the pool explicitly so the helper doesn't rebuild it per call.
func insertRun(t *testing.T, pool *pgxpool.Pool, ctx context.Context, pipelineID uuid.UUID, counter int, cause, status string) uuid.UUID {
	t.Helper()
	var runID uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO runs (pipeline_id, counter, cause, status, revisions, created_at)
		VALUES ($1, $2, $3, $4, '{}'::jsonb, now())
		RETURNING id`,
		pipelineID, counter, cause, status,
	).Scan(&runID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return runID
}

// TestListRunsGlobal_PipelineFilter is the canonical "show me every deploy
// across projects" case that motivated the pipeline_filter param. Two
// projects, each with a `deploy` pipeline + run: filtering by
// `Pipeline: "deploy"` must return BOTH.
func TestListRunsGlobal_PipelineFilter(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelines := seedTwoProjectsWithDeploy(t, s, ctx)

	// One run per pipeline (4 total), all in status=success so the
	// filter axis exercised is pipeline_name alone.
	insertRun(t, pool, ctx, pipelines["payments/build"], 1, "manual", "success")
	insertRun(t, pool, ctx, pipelines["payments/deploy"], 1, "manual", "success")
	insertRun(t, pool, ctx, pipelines["storefront/build"], 1, "manual", "success")
	insertRun(t, pool, ctx, pipelines["storefront/deploy"], 1, "manual", "success")

	runs, err := s.ListRunsGlobal(ctx, 100, 0, store.RunsFilter{Pipeline: "deploy"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2 (one deploy per project)", len(runs))
	}
	// Every returned row must be a deploy — no build slippage.
	for _, r := range runs {
		if r.PipelineName != "deploy" {
			t.Fatalf("row pipeline = %q, want deploy", r.PipelineName)
		}
	}

	// CountRunsGlobal must agree.
	n, err := s.CountRunsGlobal(ctx, store.RunsFilter{Pipeline: "deploy"})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
}

// TestListRunsGlobal_ProjectAndPipeline narrows to a single project's
// deploy — the drill-down after picking both dropdowns in /runs.
func TestListRunsGlobal_ProjectAndPipeline(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelines := seedTwoProjectsWithDeploy(t, s, ctx)
	insertRun(t, pool, ctx, pipelines["payments/deploy"], 1, "manual", "success")
	insertRun(t, pool, ctx, pipelines["storefront/deploy"], 1, "manual", "success")

	runs, err := s.ListRunsGlobal(ctx, 100, 0, store.RunsFilter{
		ProjectSlug: "payments", Pipeline: "deploy",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1 (payments/deploy only)", len(runs))
	}
	if runs[0].ProjectSlug != "payments" || runs[0].PipelineName != "deploy" {
		t.Fatalf("row = project=%q pipeline=%q, want payments/deploy",
			runs[0].ProjectSlug, runs[0].PipelineName)
	}
}

// TestListRunsGlobal_EmptyFilter proves the same query drives the
// dashboard widget (no filter). Empty strings in RunsFilter must
// return every run — no accidental filtering when a caller passes
// zero-value struct.
func TestListRunsGlobal_EmptyFilter(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelines := seedTwoProjectsWithDeploy(t, s, ctx)
	insertRun(t, pool, ctx, pipelines["payments/build"], 1, "manual", "success")
	insertRun(t, pool, ctx, pipelines["payments/deploy"], 1, "manual", "success")
	insertRun(t, pool, ctx, pipelines["storefront/build"], 1, "manual", "success")
	insertRun(t, pool, ctx, pipelines["storefront/deploy"], 1, "manual", "success")

	runs, err := s.ListRunsGlobal(ctx, 100, 0, store.RunsFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 4 {
		t.Fatalf("runs = %d, want 4 (all)", len(runs))
	}

	n, err := s.CountRunsGlobal(ctx, store.RunsFilter{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 4 {
		t.Fatalf("count = %d, want 4", n)
	}
}

// TestListPipelineNames_DistinctSorted verifies the dropdown source:
// distinct names, sorted, no duplicate `deploy` across projects.
func TestListPipelineNames_DistinctSorted(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// Two projects with the SAME pipeline names → DISTINCT should
	// dedupe to 2 (build, deploy).
	seedTwoProjectsWithDeploy(t, s, ctx)

	names, err := s.ListPipelineNames(ctx)
	if err != nil {
		t.Fatalf("list names: %v", err)
	}
	// Alphabetical: build, deploy.
	if len(names) != 2 || names[0] != "build" || names[1] != "deploy" {
		t.Fatalf("names = %v, want [build deploy]", names)
	}
}

// TestListPipelineNames_ExcludesSystemManaged proves the WHERE clause
// hides internal names (e.g. `_compliance`) from the user-facing
// dropdown. Without the guard, the synthetic pipeline the server
// spins up shows up as a selectable option, which leaks internals
// and doesn't map to anything the user can filter meaningfully.
func TestListPipelineNames_ExcludesSystemManaged(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// Regular repo pipeline — should show up.
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "app", Name: "app",
		Pipelines: []*domain.Pipeline{{
			Name: "build", Stages: []string{"check"},
			Jobs: []domain.Job{{Name: "test", Stage: "check"}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Synthesise a system_managed pipeline the way the server does
	// for _compliance (bypassing ApplyProject which never marks it).
	// definition_raw is NOT NULL — pre-policy definition the compliance
	// engine keeps; hand-rolled inserts have to fill it (ApplyProject
	// does it for the other tests).
	if _, err := pool.Exec(ctx, `
		INSERT INTO pipelines (project_id, name, definition, definition_raw, system_managed)
		VALUES ($1, '_compliance', '{}'::jsonb, '{}'::jsonb, true)`,
		res.ProjectID,
	); err != nil {
		t.Fatalf("insert system_managed: %v", err)
	}

	names, err := s.ListPipelineNames(ctx)
	if err != nil {
		t.Fatalf("list names: %v", err)
	}
	if len(names) != 1 || names[0] != "build" {
		t.Fatalf("names = %v, want [build] (system_managed excluded)", names)
	}
}
