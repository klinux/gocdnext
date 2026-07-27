package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

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
