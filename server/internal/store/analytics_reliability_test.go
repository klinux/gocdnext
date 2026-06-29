package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedRuns inserts n terminal runs for a pipeline, finished `daysAgo` days ago,
// each with a 30s queue wait (created→start) and 120s duration (start→finish)
// so the medians are deterministic.
func seedRuns(t *testing.T, pool *pgxpool.Pool, ctx context.Context, pipelineID uuid.UUID, fromCounter, n int, status string, daysAgo int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO runs (pipeline_id, counter, cause, status, revisions, created_at, started_at, finished_at)
			VALUES ($1, $2, 'manual', $3, '{}'::jsonb,
			        now() - make_interval(days => $4, secs => 150),
			        now() - make_interval(days => $4, secs => 120),
			        now() - make_interval(days => $4))`,
			pipelineID, fromCounter+i, status, daysAgo); err != nil {
			t.Fatalf("seed run: %v", err)
		}
	}
}

// applyLabeledProject applies a project with the named pipelines and a single
// label, returning pipeline IDs keyed by name (robust against apply ordering).
func applyLabeledProject(t *testing.T, s *store.Store, ctx context.Context, slug, key, value string, pipelines ...string) map[string]uuid.UUID {
	t.Helper()
	defs := make([]*domain.Pipeline, 0, len(pipelines))
	for _, name := range pipelines {
		defs = append(defs, &domain.Pipeline{
			Name: name, Stages: []string{"build"},
			Jobs: []domain.Job{{Name: "compile", Stage: "build"}},
		})
	}
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: slug, Name: slug, Pipelines: defs})
	if err != nil {
		t.Fatalf("apply %s: %v", slug, err)
	}
	if err := s.ReplaceProjectLabels(ctx, res.ProjectID, []store.ProjectLabel{{Key: key, Value: value}}); err != nil {
		t.Fatalf("labels %s: %v", slug, err)
	}
	ids := make(map[string]uuid.UUID, len(res.Pipelines))
	for _, p := range res.Pipelines {
		ids[p.Name] = uuid.UUID(p.PipelineID)
	}
	return ids
}

func TestReliabilityReport(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// payments project: main (8 ok / 2 fail / 1 errored / 1 canceled),
	// flaky (1 ok / 1 fail), clean (6 ok).
	pay := applyLabeledProject(t, s, ctx, "pay", "team", "payments", "main", "flaky", "clean")
	seedRuns(t, pool, ctx, pay["main"], 1, 8, "success", 1)
	seedRuns(t, pool, ctx, pay["main"], 9, 2, "failed", 1)
	seedRuns(t, pool, ctx, pay["main"], 11, 1, "errored", 1)  // counts as a failure
	seedRuns(t, pool, ctx, pay["main"], 12, 1, "canceled", 1) // excluded from the rate
	seedRuns(t, pool, ctx, pay["flaky"], 1, 1, "success", 1)
	seedRuns(t, pool, ctx, pay["flaky"], 2, 1, "failed", 1)
	seedRuns(t, pool, ctx, pay["clean"], 1, 6, "success", 1)

	// storefront project: web (3 ok / 5 fail) — the worst offender.
	shop := applyLabeledProject(t, s, ctx, "shop", "team", "storefront", "web")
	seedRuns(t, pool, ctx, shop["web"], 1, 3, "success", 1)
	seedRuns(t, pool, ctx, shop["web"], 4, 5, "failed", 1)

	// An old burst (40d ago) must fall outside the 30-day window entirely.
	seedRuns(t, pool, ctx, pay["clean"], 100, 50, "failed", 40)

	rep, err := s.ReliabilityReport(ctx, "team", 30)
	if err != nil {
		t.Fatalf("reliability: %v", err)
	}

	// --- throughput groups ---
	byGroup := map[string]store.ThroughputGroup{}
	for _, g := range rep.Groups {
		byGroup[g.Group] = g
	}
	if len(byGroup) != 2 {
		t.Fatalf("groups = %+v", rep.Groups)
	}

	// payments terminal runs in window: main 11 (8 ok + 2 fail + 1 err; canceled
	// & the 40d burst excluded), flaky 2 (1/1), clean 6 (6/0).
	// → success 15, failed 4, total 19.
	pg := byGroup["payments"]
	if pg.RunsSuccess != 15 || pg.RunsFailed != 4 || pg.RunsTotal != 19 {
		t.Fatalf("payments counts = %+v", pg)
	}
	if math.Abs(pg.SuccessRate-15.0/19.0) > 1e-6 {
		t.Errorf("payments success rate = %v, want %v", pg.SuccessRate, 15.0/19.0)
	}
	if math.Abs(pg.RunsPerDay-19.0/30.0) > 1e-6 {
		t.Errorf("payments runs/day = %v, want %v", pg.RunsPerDay, 19.0/30.0)
	}
	if math.Abs(pg.QueueWaitP50Sec-30) > 1 || math.Abs(pg.DurationP50Sec-120) > 1 {
		t.Errorf("payments queue/dur p50 = %v/%v, want ~30/120", pg.QueueWaitP50Sec, pg.DurationP50Sec)
	}

	sg := byGroup["storefront"]
	if sg.RunsSuccess != 3 || sg.RunsFailed != 5 || sg.RunsTotal != 8 {
		t.Fatalf("storefront counts = %+v", sg)
	}

	// --- reliability hotspots ---
	// Qualifiers (≥5 terminal runs + ≥1 failure): shop/web (5/8 = .625),
	// pay/main (3/11 ≈ .273). pay/flaky (2 runs < 5) and pay/clean (0 failures
	// in window) are excluded.
	if len(rep.Hotspots) != 2 {
		t.Fatalf("hotspots = %+v", rep.Hotspots)
	}
	if rep.Hotspots[0].Pipeline != "web" || rep.Hotspots[0].Project != "shop" {
		t.Fatalf("worst hotspot = %+v, want shop/web", rep.Hotspots[0])
	}
	if math.Abs(rep.Hotspots[0].FailureRate-5.0/8.0) > 1e-6 {
		t.Errorf("web failure rate = %v, want %v", rep.Hotspots[0].FailureRate, 5.0/8.0)
	}
	if rep.Hotspots[1].Pipeline != "main" {
		t.Fatalf("second hotspot = %+v, want pay/main", rep.Hotspots[1])
	}
	if math.Abs(rep.Hotspots[1].FailureRate-3.0/11.0) > 1e-6 {
		t.Errorf("main failure rate = %v, want %v", rep.Hotspots[1].FailureRate, 3.0/11.0)
	}
}
