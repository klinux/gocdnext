package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestListProjects_EmptyWhenNoProjects(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	got, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %d", len(got))
	}
}

func TestListProjects_CountsAndLatestRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	if _, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1)); err != nil {
		t.Fatalf("create run: %v", err)
	}

	got, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 project, got %d", len(got))
	}
	p := got[0]
	if p.Slug != "demo" || p.Name != "Demo" {
		t.Fatalf("project = %+v", p)
	}
	if p.PipelineCount != 1 {
		t.Fatalf("PipelineCount = %d, want 1", p.PipelineCount)
	}
	if p.LatestRunAt == nil {
		t.Fatalf("LatestRunAt should be set after first run")
	}
	if time.Since(*p.LatestRunAt) > time.Minute {
		t.Fatalf("LatestRunAt looks stale: %v", p.LatestRunAt)
	}

	// After a run is created the preview should surface it so the
	// projects page card can render the status node instead of the
	// grey "never run" pill. Regression guard for the list-card vs
	// detail-card parity the user called out.
	if len(p.TopPipelines) != 1 {
		t.Fatalf("TopPipelines = %d, want 1", len(p.TopPipelines))
	}
	tp := p.TopPipelines[0]
	if tp.LatestRunStatus == "" {
		t.Fatalf("TopPipelines[0].LatestRunStatus is empty — run wasn't attached")
	}
	if len(tp.LatestRunStages) == 0 {
		t.Fatalf("TopPipelines[0].LatestRunStages is empty — stage_runs weren't attached")
	}
}

func TestGetProjectDetail_NotFound(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	_, err := s.GetProjectDetail(context.Background(), "nope", 10)
	if !errors.Is(err, store.ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

func TestGetProjectDetail_ReturnsPipelinesAndRuns(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run1, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	in2 := baseTriggerInput(pipelineID, materialID, 2)
	in2.Revision = "b111111111111111111111111111111111111111"
	if _, err := s.CreateRunFromModification(ctx, in2); err != nil {
		t.Fatalf("run2: %v", err)
	}

	got, err := s.GetProjectDetail(ctx, "demo", 10)
	if err != nil {
		t.Fatalf("GetProjectDetail: %v", err)
	}
	if got.Project.Slug != "demo" {
		t.Fatalf("project slug = %q", got.Project.Slug)
	}
	if len(got.Pipelines) != 1 || got.Pipelines[0].Name != "build" {
		t.Fatalf("pipelines = %+v", got.Pipelines)
	}
	if len(got.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(got.Runs))
	}
	// Most recent first — run2 was second insert, so it should lead.
	if got.Runs[0].Counter != 2 || got.Runs[1].Counter != 1 {
		t.Fatalf("run order = %+v", got.Runs)
	}
	if got.Runs[1].ID != run1.RunID {
		t.Fatalf("run ids mismatch")
	}
}

func TestGetProjectDetail_MetricsNilWhenNoTerminalRuns(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	// Freshly-created run is queued — not a terminal status, so the
	// metrics aggregate should stay unset. Regression guard: a
	// COALESCE(..., 0) elsewhere could easily let zeroed stats leak
	// through as if they were real values.
	if _, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1)); err != nil {
		t.Fatalf("create run: %v", err)
	}

	got, err := s.GetProjectDetail(ctx, "demo", 10)
	if err != nil {
		t.Fatalf("GetProjectDetail: %v", err)
	}
	if got.Pipelines[0].Metrics != nil {
		t.Fatalf("Metrics populated without terminal runs: %+v", got.Pipelines[0].Metrics)
	}
}

func TestGetProjectDetail_MetricsAggregatesTerminalRuns(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	r1, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	in2 := baseTriggerInput(pipelineID, materialID, 2)
	in2.Revision = "b111111111111111111111111111111111111111"
	r2, err := s.CreateRunFromModification(ctx, in2)
	if err != nil {
		t.Fatalf("run2: %v", err)
	}

	// Mark both runs as finished with a known duration so the p50
	// math is predictable. Direct SQL because the production code
	// path to terminal status is orchestration-driven (stage
	// completion → run update) — way more setup than a metrics
	// assertion needs.
	now := time.Now().UTC()
	mark := func(runID [16]byte, status string, leadSec int) {
		t.Helper()
		start := now.Add(-time.Duration(leadSec+60) * time.Second)
		end := start.Add(time.Duration(leadSec) * time.Second)
		if _, err := pool.Exec(ctx,
			`UPDATE runs SET status=$1, started_at=$2, finished_at=$3 WHERE id=$4`,
			status, start, end, runID,
		); err != nil {
			t.Fatalf("update run: %v", err)
		}
		// Stage timings: build half + test half, so the sum equals
		// the overall run duration. Process time p50 should match
		// lead time p50 in this fixture.
		mid := start.Add(time.Duration(leadSec/2) * time.Second)
		if _, err := pool.Exec(ctx,
			`UPDATE stage_runs SET status='success', started_at=$1, finished_at=$2 WHERE run_id=$3 AND ordinal=0`,
			start, mid, runID,
		); err != nil {
			t.Fatalf("update stage build: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE stage_runs SET status=$1, started_at=$2, finished_at=$3 WHERE run_id=$4 AND ordinal=1`,
			status, mid, end, runID,
		); err != nil {
			t.Fatalf("update stage test: %v", err)
		}
	}
	mark(r1.RunID, "success", 60)
	mark(r2.RunID, "failed", 120)

	got, err := s.GetProjectDetail(ctx, "demo", 10)
	if err != nil {
		t.Fatalf("GetProjectDetail: %v", err)
	}
	m := got.Pipelines[0].Metrics
	if m == nil {
		t.Fatalf("Metrics nil after two terminal runs")
	}
	if m.RunsConsidered != 2 {
		t.Fatalf("RunsConsidered = %d, want 2", m.RunsConsidered)
	}
	if m.SuccessRate != 0.5 {
		t.Fatalf("SuccessRate = %v, want 0.5", m.SuccessRate)
	}
	// p50 of {60, 120} = 90. Allow small float slack.
	if m.LeadTimeP50Sec < 89 || m.LeadTimeP50Sec > 91 {
		t.Fatalf("LeadTimeP50Sec = %v, want ~90", m.LeadTimeP50Sec)
	}
	if len(m.StageStats) != 2 {
		t.Fatalf("StageStats = %d, want 2", len(m.StageStats))
	}
}

func TestGetRunDetail_NotFound(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	_, err := s.GetRunDetail(context.Background(), nonexistentUUID(), 0)
	if !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
}

func TestGetRunDetail_StagesJobsAndOptionalLogs(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Attach a log line to compile so the logs tail path is exercised.
	compileID := run.JobRuns[0].ID
	if err := s.InsertLogLine(ctx, store.LogLine{
		JobRunID: compileID, Seq: 1, Stream: "stdout",
		At: time.Now().UTC(), Text: "hello world",
	}); err != nil {
		t.Fatalf("log: %v", err)
	}

	got, err := s.GetRunDetail(ctx, run.RunID, 50)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if got.RunSummary.ID != run.RunID {
		t.Fatalf("id mismatch")
	}
	if got.ProjectSlug != "demo" {
		t.Fatalf("project_slug = %q", got.ProjectSlug)
	}
	if len(got.Stages) != 2 {
		t.Fatalf("stages = %d", len(got.Stages))
	}
	if got.Stages[0].Name != "build" || got.Stages[1].Name != "test" {
		t.Fatalf("stage order: %+v", got.Stages)
	}
	if len(got.Stages[0].Jobs) != 1 || got.Stages[0].Jobs[0].Name != "compile" {
		t.Fatalf("build jobs: %+v", got.Stages[0].Jobs)
	}

	var found bool
	for _, j := range got.Stages[0].Jobs {
		if j.ID == compileID {
			found = true
			if len(j.Logs) != 1 || j.Logs[0].Text != "hello world" {
				t.Fatalf("logs = %+v", j.Logs)
			}
		}
	}
	if !found {
		t.Fatalf("compile job missing")
	}
}

func TestGetRunDetail_LogsSkippedWhenLimitZero(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := s.InsertLogLine(ctx, store.LogLine{
		JobRunID: run.JobRuns[0].ID, Seq: 1, Stream: "stdout",
		At: time.Now().UTC(), Text: "x",
	}); err != nil {
		t.Fatalf("log: %v", err)
	}

	got, err := s.GetRunDetail(ctx, run.RunID, 0)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	for _, st := range got.Stages {
		for _, j := range st.Jobs {
			if len(j.Logs) != 0 {
				t.Fatalf("logs populated despite limit=0: %+v", j.Logs)
			}
		}
	}
}

func nonexistentUUID() (u [16]byte) {
	// Deterministic, doesn't need to be random.
	u[0] = 0xff
	return u
}
