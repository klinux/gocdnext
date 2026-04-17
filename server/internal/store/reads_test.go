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
