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
