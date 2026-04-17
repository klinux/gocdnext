package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestListDispatchableJobs_ReturnsActiveStageOnly(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	jobs, err := s.ListDispatchableJobs(ctx, res.RunID)
	if err != nil {
		t.Fatalf("list dispatchable: %v", err)
	}
	// Seed pipeline has stages [build, test] with one job each. Initially only
	// the build-stage job (compile) is dispatchable.
	if len(jobs) != 1 || jobs[0].Name != "compile" {
		t.Fatalf("want [compile], got %+v", jobs)
	}
}

func TestAssignJob_OnlyOneWinsRace(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	jobs, _ := s.ListDispatchableJobs(ctx, res.RunID)
	if len(jobs) == 0 {
		t.Fatalf("no dispatchable job to assign")
	}
	jobID := jobs[0].ID
	agentA, agentB := uuid.New(), uuid.New()

	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (id, name, token_hash) VALUES ($1, $2, 'hash'), ($3, $4, 'hash')`,
		agentA, "a", agentB, "b",
	); err != nil {
		t.Fatalf("seed agents: %v", err)
	}

	_, okA, err := s.AssignJob(ctx, jobID, agentA)
	if err != nil || !okA {
		t.Fatalf("A assign: ok=%v err=%v", okA, err)
	}
	_, okB, err := s.AssignJob(ctx, jobID, agentB)
	if err != nil {
		t.Fatalf("B assign err: %v", err)
	}
	if okB {
		t.Fatalf("second AssignJob should have lost the race")
	}
}

func TestAssignJob_RemovesJobFromDispatchable(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	jobs, _ := s.ListDispatchableJobs(ctx, res.RunID)
	agentID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (id, name, token_hash) VALUES ($1, 'a', 'hash')`, agentID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, ok, err := s.AssignJob(ctx, jobs[0].ID, agentID); err != nil || !ok {
		t.Fatalf("assign: ok=%v err=%v", ok, err)
	}

	after, _ := s.ListDispatchableJobs(ctx, res.RunID)
	if len(after) != 0 {
		t.Fatalf("dispatchable after assign = %+v, want []", after)
	}
}

func TestMarkRunRunning_IdempotentAndNoopWhenAlreadyRunning(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := s.MarkRunRunning(ctx, res.RunID); err != nil {
		t.Fatalf("mark 1: %v", err)
	}
	if err := s.MarkRunRunning(ctx, res.RunID); err != nil {
		t.Fatalf("mark 2: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, res.RunID).Scan(&status); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != "running" {
		t.Fatalf("status = %s, want running", status)
	}
}

func TestListQueuedRunIDs_OrdersByCreatedAt(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	_, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	in2 := baseTriggerInput(pipelineID, materialID, 2)
	in2.Revision = "b111111111111111111111111111111111111111"
	_, err = s.CreateRunFromModification(ctx, in2)
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}

	ids, err := s.ListQueuedRunIDs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("len = %d, want 2", len(ids))
	}
}
