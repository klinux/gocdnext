package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedSerialPipeline seeds a Concurrency=serial pipeline with a single stage and
// job, so a run's whole dispatchability hinges on the serial gate, not stages.
func seedSerialPipeline(t *testing.T, pool *pgxpool.Pool) (pipelineID, materialID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	url, branch := "https://github.com/org/serial", "main"
	fp := store.FingerprintFor(url, branch)
	p := &domain.Pipeline{
		Name:        "serial-build",
		Concurrency: domain.ConcurrencySerial,
		Stages:      []string{"build"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
		}},
		Jobs: []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
	}
	ctx := context.Background()
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "serial-demo", Name: "Serial", Pipelines: []*domain.Pipeline{p},
	})
	if err != nil {
		t.Fatalf("seed serial apply: %v", err)
	}
	pipelineID = res.Pipelines[0].PipelineID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("seed serial material lookup: %v", err)
	}
	return
}

// The dispatchable count is the autoscaling signal (#185): it must count only
// job_runs that genuinely want an agent right now — queued, unassigned, not an
// approval gate, and in their run's ACTIVE (lowest-ordinal non-terminal) stage.
// A raw `status='queued'` count would be wrong: every job_run is created queued
// upfront (all stages), so a future-stage job would inflate the fleet.
func TestGetQueueDepth_DispatchableExcludesFutureStagesAssignedAndGates(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Seed has stages [build(0), test(1)] with compile@build and unit@test, both
	// created 'queued'. Only compile is in the active stage → dispatchable is 1,
	// NOT the raw queued count of 2. This is the whole point of the metric.
	snap, err := s.GetQueueDepth(ctx)
	if err != nil {
		t.Fatalf("queue depth: %v", err)
	}
	if snap.DispatchableJobs != 1 {
		t.Fatalf("dispatchable = %d, want 1 (active-stage compile only; test-stage unit is future)", snap.DispatchableJobs)
	}

	// An approval gate queued in the ACTIVE stage is a state transition, never
	// handed to an agent — it must not inflate the signal.
	var buildStage uuid.UUID
	for _, sr := range res.StageRuns {
		if sr.Name == "build" {
			buildStage = sr.ID
		}
	}
	if buildStage == uuid.Nil {
		t.Fatalf("build stage not found in %+v", res.StageRuns)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO job_runs (run_id, stage_run_id, name, status, approval_gate)
		 VALUES ($1, $2, 'gate', 'queued', true)`,
		res.RunID, buildStage,
	); err != nil {
		t.Fatalf("seed gate: %v", err)
	}
	if snap, err = s.GetQueueDepth(ctx); err != nil {
		t.Fatalf("queue depth after gate: %v", err)
	}
	if snap.DispatchableJobs != 1 {
		t.Fatalf("dispatchable = %d after gate insert, want 1 (approval gate excluded)", snap.DispatchableJobs)
	}

	// Assigning compile moves it to running → dispatchable drops to 0 (unit is
	// still future, the gate never counts).
	agent := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (id, name, token_hash) VALUES ($1, 'a', 'h')`, agent,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	jobs, err := s.ListDispatchableJobs(ctx, res.RunID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("dispatchable jobs = %d err=%v, want [compile]", len(jobs), err)
	}
	if _, ok, err := s.AssignJob(ctx, jobs[0].ID, agent); err != nil || !ok {
		t.Fatalf("assign compile: ok=%v err=%v", ok, err)
	}
	if snap, err = s.GetQueueDepth(ctx); err != nil {
		t.Fatalf("queue depth after assign: %v", err)
	}
	if snap.DispatchableJobs != 0 {
		t.Fatalf("dispatchable = %d after assigning compile, want 0", snap.DispatchableJobs)
	}
}

// A serial pipeline dispatches one run at a time: a queued run sitting behind a
// running sibling can't hand its jobs to an agent, so it must NOT inflate the
// autoscaling signal (the scheduler's serial gate, mirrored in SQL).
func TestGetQueueDepth_DispatchableExcludesSerialGatedRuns(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID := seedSerialPipeline(t, pool)
	mk := func(mod int64, rev, delivery string) store.CreateRunFromModificationInput {
		return store.CreateRunFromModificationInput{
			PipelineID: pipelineID, MaterialID: materialID, ModificationID: mod,
			Revision: rev, Branch: "main", Provider: "github",
			Delivery: delivery, TriggeredBy: "system:webhook",
		}
	}
	run1, err := s.CreateRunFromModification(ctx, mk(1, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "d1"))
	if err != nil {
		t.Fatalf("create run1: %v", err)
	}
	if _, err := s.CreateRunFromModification(ctx, mk(2, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "d2")); err != nil {
		t.Fatalf("create run2: %v", err)
	}

	// Both queued, none running yet: the serial gate only fires against a RUNNING
	// sibling (mirrors OtherRunningRunForPipeline), so both fronts count. In
	// production refreshQueueDepth runs right after drainQueued, which starts one
	// and gates the rest, so this pre-start state never lingers.
	snap, err := s.GetQueueDepth(ctx)
	if err != nil {
		t.Fatalf("queue depth: %v", err)
	}
	if snap.DispatchableJobs != 2 {
		t.Fatalf("dispatchable = %d with both runs queued, want 2", snap.DispatchableJobs)
	}

	// Start run1 (its predecessor slot). run2 is now serial-gated behind a running
	// sibling → its compile drops out; run1's own compile still counts. => 1, not 2.
	if _, err := pool.Exec(ctx, `UPDATE runs SET status = 'running' WHERE id = $1`, run1.RunID); err != nil {
		t.Fatalf("mark run1 running: %v", err)
	}
	if snap, err = s.GetQueueDepth(ctx); err != nil {
		t.Fatalf("queue depth after run1 running: %v", err)
	}
	if snap.DispatchableJobs != 1 {
		t.Fatalf("dispatchable = %d with run2 serial-gated behind running run1, want 1", snap.DispatchableJobs)
	}
}

// Merge-group runs keep the pipeline's serial contract: if a prior sibling is
// already running, their jobs are not dispatchable yet and must not inflate the
// autoscaling signal.
func TestGetQueueDepth_DispatchableExcludesMergeGroupSerialRuns(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	fp := store.FingerprintFor("https://github.com/org/serial", "main")

	pipelineID, materialID := seedSerialPipeline(t, pool)
	run1, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     pipelineID,
		MaterialID:     materialID,
		ModificationID: 1,
		Revision:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Branch:         "main",
		Provider:       "github",
		Delivery:       "d1",
		TriggeredBy:    "system:webhook",
	})
	if err != nil {
		t.Fatalf("create run1: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runs SET status = 'running' WHERE id = $1`, run1.RunID); err != nil {
		t.Fatalf("mark run1 running: %v", err)
	}

	detail, _ := json.Marshal(map[string]any{
		"mg_head_sha":    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"mg_head_ref":    "gh-readonly-queue/main/pr-2-bbbb",
		"mg_base_sha":    "1111111111111111111111111111111111111111",
		"mg_base_ref":    "main",
		"mg_fingerprint": fp,
	})
	if _, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     pipelineID,
		MaterialID:     materialID,
		ModificationID: 2,
		Revision:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Branch:         "gh-readonly-queue/main/pr-2-bbbb",
		Provider:       "github",
		Delivery:       "d2",
		TriggeredBy:    "system:webhook",
		Cause:          string(domain.CauseMergeGroup),
		CauseDetail:    detail,
	}); err != nil {
		t.Fatalf("create merge_group run: %v", err)
	}

	snap, err := s.GetQueueDepth(ctx)
	if err != nil {
		t.Fatalf("queue depth: %v", err)
	}
	if snap.DispatchableJobs != 1 {
		t.Fatalf("dispatchable = %d with merge_group behind serial run, want 1", snap.DispatchableJobs)
	}
}
