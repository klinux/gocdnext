package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestWriteCoverage_HappyPathAndRerunUpsert(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)

	in := store.CoverageIn{
		Format:       "go-cover",
		LinesCovered: 70,
		LinesTotal:   100,
		Packages: []store.PackageCoverageIn{
			{Name: "internal/cart", LinesCovered: 30, LinesTotal: 50},
			{Name: "internal/pay", LinesCovered: 40, LinesTotal: 50},
		},
	}
	if err := s.WriteCoverage(ctx, jobID, agentID, 0, in); err != nil {
		t.Fatalf("WriteCoverage: %v", err)
	}

	rows, err := s.CoverageByRun(ctx, runID)
	if err != nil {
		t.Fatalf("CoverageByRun: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].LinesCovered != 70 || rows[0].LinesTotal != 100 || len(rows[0].Packages) != 2 {
		t.Fatalf("row = %+v", rows[0])
	}

	// Rerun of the same job_run (same attempt for the test) rewrites
	// the SAME row — never a second one.
	in.LinesCovered = 80
	if err := s.WriteCoverage(ctx, jobID, agentID, 0, in); err != nil {
		t.Fatalf("WriteCoverage rerun: %v", err)
	}
	rows, _ = s.CoverageByRun(ctx, runID)
	if len(rows) != 1 || rows[0].LinesCovered != 80 {
		t.Fatalf("upsert broken: %+v", rows)
	}
}

func TestWriteCoverage_RejectsStaleSnapshot(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)
	var newAgent uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash, status, last_seen_at)
		 VALUES ('cov-new', 'h', 'online', NOW()) RETURNING id`,
	).Scan(&newAgent); err != nil {
		t.Fatalf("seed new agent: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET agent_id=$1, attempt=attempt+1 WHERE id=$2`,
		newAgent, jobID,
	); err != nil {
		t.Fatalf("simulate redispatch: %v", err)
	}

	err := s.WriteCoverage(ctx, jobID, agentID, 0, store.CoverageIn{
		Format: "go-cover", LinesCovered: 1, LinesTotal: 1,
	})
	if !errors.Is(err, store.ErrSnapshotStale) {
		t.Fatalf("err = %v, want ErrSnapshotStale", err)
	}
	rows, _ := s.CoverageByRun(ctx, runID)
	if len(rows) != 0 {
		t.Fatalf("stale write landed: %+v", rows)
	}
}

func TestCoverageTrend(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)
	if err := s.WriteCoverage(ctx, jobID, agentID, 0, store.CoverageIn{
		Format: "go-cover", LinesCovered: 50, LinesTotal: 100,
	}); err != nil {
		t.Fatalf("WriteCoverage: %v", err)
	}

	var pipelineID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT pipeline_id FROM runs WHERE id=$1`, runID,
	).Scan(&pipelineID); err != nil {
		t.Fatalf("pipeline lookup: %v", err)
	}
	points, err := s.CoverageTrend(ctx, pipelineID, 10)
	if err != nil {
		t.Fatalf("CoverageTrend: %v", err)
	}
	if len(points) != 1 || points[0].RunID != runID || points[0].LinesTotal != 100 {
		t.Fatalf("points = %+v", points)
	}
}

// Review-round MEDIUM: the trend window is PER SERIES — a global
// LIMIT let one chatty job starve every other sparkline. With two
// jobs reporting and limit=1, BOTH series must return one point.
func TestCoverageTrend_PerSeriesWindow(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)
	if err := s.WriteCoverage(ctx, jobID, agentID, 0, store.CoverageIn{
		Format: "go-cover", LinesCovered: 50, LinesTotal: 100,
	}); err != nil {
		t.Fatalf("WriteCoverage job1: %v", err)
	}

	// Second job in the same run, different name → second series.
	var stageID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT stage_run_id FROM job_runs WHERE id=$1`, jobID,
	).Scan(&stageID); err != nil {
		t.Fatalf("stage lookup: %v", err)
	}
	var job2 uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO job_runs (run_id, stage_run_id, name, status, agent_id, attempt)
		 VALUES ($1, $2, 'integration', 'running', $3, 0) RETURNING id`,
		runID, stageID, agentID,
	).Scan(&job2); err != nil {
		t.Fatalf("seed job2: %v", err)
	}
	if err := s.WriteCoverage(ctx, job2, agentID, 0, store.CoverageIn{
		Format: "go-cover", LinesCovered: 80, LinesTotal: 100,
	}); err != nil {
		t.Fatalf("WriteCoverage job2: %v", err)
	}

	var pipelineID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT pipeline_id FROM runs WHERE id=$1`, runID,
	).Scan(&pipelineID); err != nil {
		t.Fatalf("pipeline lookup: %v", err)
	}
	points, err := s.CoverageTrend(ctx, pipelineID, 1)
	if err != nil {
		t.Fatalf("CoverageTrend: %v", err)
	}
	names := map[string]bool{}
	for _, p := range points {
		names[p.JobName] = true
	}
	if len(points) != 2 || !names["integration"] {
		t.Fatalf("points = %+v — per-series window broken (one series starved)", points)
	}
}

// Phase 2: PR runs get the latest MAINLINE (webhook/poll-cause)
// measurement of the same series as baseline; runs without a
// mainline predecessor get none.
func TestCoverageByRun_BaselineFromMainline(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// Run 1 = the mainline with 50% coverage. NO cause injection:
	// the seed creates the run through the store path, which
	// defaults branch pushes to cause='webhook' — the test must
	// exercise the state production actually creates (review-round
	// HIGH: the original test injected a fabricated cause='push'
	// and masked a filter that matched nothing real).
	jobID, agentID, runID := seedRunningAgentJob(t, pool)
	var cause string
	if err := pool.QueryRow(ctx, `SELECT cause FROM runs WHERE id=$1`, runID).Scan(&cause); err != nil || cause != "webhook" {
		t.Fatalf("seed precondition: cause = %q (err=%v), want webhook — seed changed, revisit baseline semantics", cause, err)
	}
	if err := s.WriteCoverage(ctx, jobID, agentID, 0, store.CoverageIn{
		Format: "go-cover", LinesCovered: 50, LinesTotal: 100,
	}); err != nil {
		t.Fatalf("WriteCoverage mainline: %v", err)
	}

	// Run 2 = the PR run, same pipeline + same job name, 70%.
	var pipelineID, stageID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT pipeline_id FROM runs WHERE id=$1`, runID).Scan(&pipelineID); err != nil {
		t.Fatalf("pipeline lookup: %v", err)
	}
	var jobName string
	if err := pool.QueryRow(ctx,
		`SELECT name, stage_run_id FROM job_runs WHERE id=$1`, jobID,
	).Scan(&jobName, &stageID); err != nil {
		t.Fatalf("job lookup: %v", err)
	}
	var run2 uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (pipeline_id, counter, status, cause, revisions)
		 VALUES ($1, 999, 'running', 'pull_request', '{}') RETURNING id`, pipelineID,
	).Scan(&run2); err != nil {
		t.Fatalf("seed run2: %v", err)
	}
	var stage2 uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO stage_runs (run_id, name, ordinal, status)
		 VALUES ($1, 'test', 0, 'running') RETURNING id`, run2,
	).Scan(&stage2); err != nil {
		t.Fatalf("seed stage2: %v", err)
	}
	var job2 uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO job_runs (run_id, stage_run_id, name, status, agent_id, attempt)
		 VALUES ($1, $2, $3, 'running', $4, 0) RETURNING id`,
		run2, stage2, jobName, agentID,
	).Scan(&job2); err != nil {
		t.Fatalf("seed job2: %v", err)
	}
	if err := s.WriteCoverage(ctx, job2, agentID, 0, store.CoverageIn{
		Format: "go-cover", LinesCovered: 70, LinesTotal: 100,
	}); err != nil {
		t.Fatalf("WriteCoverage pr: %v", err)
	}

	// PR run sees the mainline run as baseline.
	rows, err := s.CoverageByRun(ctx, run2)
	if err != nil {
		t.Fatalf("CoverageByRun: %v", err)
	}
	if len(rows) != 1 || rows[0].Baseline == nil {
		t.Fatalf("rows = %+v — PR run should carry a baseline", rows)
	}
	if rows[0].Baseline.LinesCovered != 50 || rows[0].Baseline.RunID != runID {
		t.Fatalf("baseline = %+v, want the mainline run's 50/100", rows[0].Baseline)
	}

	// The mainline run itself excludes itself — no other mainline
	// run exists, so it gets NO baseline (never self-compares).
	rows, err = s.CoverageByRun(ctx, runID)
	if err != nil {
		t.Fatalf("CoverageByRun mainline: %v", err)
	}
	if len(rows) != 1 || rows[0].Baseline != nil {
		t.Fatalf("mainline rows = %+v — must not self-baseline", rows)
	}
}
