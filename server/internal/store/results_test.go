package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedRunningJob sets up a run with two stages (build/test, one job each) and
// flips the first job to 'running' so CompleteJob has something to work on.
// Returns the job_run_id + related ids needed by assertions.
func seedRunningJob(t *testing.T, pool *pgxpool.Pool) (runID, stageBuildID, stageTestID, jobCompileID, jobUnitID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	runID = res.RunID
	for _, st := range res.StageRuns {
		switch st.Name {
		case "build":
			stageBuildID = st.ID
		case "test":
			stageTestID = st.ID
		}
	}
	for _, j := range res.JobRuns {
		switch j.Name {
		case "compile":
			jobCompileID = j.ID
		case "unit":
			jobUnitID = j.ID
		}
	}

	// Fake agent + flip compile → running (simulates scheduler dispatch).
	var agentID uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ($1, 'hash') RETURNING id`, "a-"+runID.String()[:8],
	).Scan(&agentID)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW() WHERE id=$2`,
		agentID, jobCompileID,
	); err != nil {
		t.Fatalf("flip compile running: %v", err)
	}
	return
}

func TestInsertLogLine_IsIdempotent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, _, _, jobID, _ := seedRunningJob(t, pool)
	line := store.LogLine{JobRunID: jobID, Seq: 1, Stream: "stdout", Text: "hello", At: time.Now().UTC()}

	if err := s.InsertLogLine(ctx, line); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := s.InsertLogLine(ctx, line); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	var count int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM log_lines WHERE job_run_id = $1`, jobID).Scan(&count)
	if count != 1 {
		t.Fatalf("log_lines count = %d, want 1 (ON CONFLICT DO NOTHING)", count)
	}
}

func TestCompleteJob_StageAndRunPromoteOnSuccess(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, stageBuildID, stageTestID, jobCompileID, jobUnitID := seedRunningJob(t, pool)

	// Complete the build-stage job.
	got, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: jobCompileID, Status: "success", ExitCode: 0,
	})
	if err != nil || !ok {
		t.Fatalf("complete compile: ok=%v err=%v", ok, err)
	}
	if !got.StageCompleted || got.StageStatus != "success" {
		t.Fatalf("stage after compile: %+v", got)
	}
	if got.RunCompleted {
		t.Fatalf("run should still be running (2nd stage pending): %+v", got)
	}

	// Flip unit → running and complete it.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='running', started_at=NOW(),
		 agent_id=(SELECT id FROM agents LIMIT 1) WHERE id=$1`,
		jobUnitID,
	); err != nil {
		t.Fatalf("flip unit running: %v", err)
	}

	got2, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: jobUnitID, Status: "success", ExitCode: 0,
	})
	if err != nil || !ok {
		t.Fatalf("complete unit: ok=%v err=%v", ok, err)
	}
	if !got2.StageCompleted || got2.StageStatus != "success" {
		t.Fatalf("stage after unit: %+v", got2)
	}
	if !got2.RunCompleted || got2.RunStatus != "success" {
		t.Fatalf("run after unit: %+v", got2)
	}

	// Confirm DB state lines up with the return values.
	assertStatus(t, pool, "stage_runs", stageBuildID, "success")
	assertStatus(t, pool, "stage_runs", stageTestID, "success")
	assertStatus(t, pool, "runs", runID, "success")
}

func TestCompleteJob_FailedJobFailsStageAndCancelsRest(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, stageBuildID, stageTestID, jobCompileID, jobUnitID := seedRunningJob(t, pool)

	got, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: jobCompileID, Status: "failed", ExitCode: 1, ErrorMsg: "boom",
	})
	if err != nil || !ok {
		t.Fatalf("complete compile: ok=%v err=%v", ok, err)
	}
	if !got.StageCompleted || got.StageStatus != "failed" {
		t.Fatalf("stage: %+v", got)
	}
	if !got.RunCompleted || got.RunStatus != "failed" {
		t.Fatalf("run: %+v", got)
	}

	assertStatus(t, pool, "stage_runs", stageBuildID, "failed")
	assertStatus(t, pool, "stage_runs", stageTestID, "canceled")
	assertStatus(t, pool, "job_runs", jobUnitID, "canceled")
	assertStatus(t, pool, "runs", runID, "failed")
}

func TestCompleteJob_DuplicateResultIsNoOp(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, _, _, jobCompileID, _ := seedRunningJob(t, pool)
	if _, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: jobCompileID, Status: "success",
	}); err != nil || !ok {
		t.Fatalf("first complete: ok=%v err=%v", ok, err)
	}
	_, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: jobCompileID, Status: "success",
	})
	if err != nil {
		t.Fatalf("second complete err: %v", err)
	}
	if ok {
		t.Fatalf("second complete should report ok=false (already terminal)")
	}
}

func TestCompleteJob_MixedMatrixFailureFailsStage(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, true)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Flip all jobs in stage 'build' to running so we can complete them.
	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ($1, 'hash')`, "a-matrix"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE job_runs
		SET status='running', started_at=NOW(),
		    agent_id=(SELECT id FROM agents WHERE name='a-matrix')
		WHERE run_id=$1
		  AND stage_run_id=(SELECT id FROM stage_runs WHERE run_id=$1 AND ordinal=0)`,
		res.RunID,
	); err != nil {
		t.Fatalf("flip stage-0 running: %v", err)
	}

	// The stage-0 seed has only "compile" — fail it, the run should terminate.
	var compileID uuid.UUID
	_ = pool.QueryRow(ctx,
		`SELECT id FROM job_runs WHERE run_id=$1 AND name='compile' LIMIT 1`, res.RunID,
	).Scan(&compileID)
	got, _, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: compileID, Status: "failed", ExitCode: 2,
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got.RunStatus != "failed" {
		t.Fatalf("run status = %q, want failed", got.RunStatus)
	}
}

func assertStatus(t *testing.T, pool *pgxpool.Pool, table string, id uuid.UUID, want string) {
	t.Helper()
	var got string
	err := pool.QueryRow(context.Background(),
		"SELECT status FROM "+table+" WHERE id = $1", id,
	).Scan(&got)
	if err != nil {
		t.Fatalf("status lookup %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s.status = %q, want %q", table, got, want)
	}
}
