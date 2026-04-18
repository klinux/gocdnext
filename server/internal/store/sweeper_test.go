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

// seedRunningAgentJob spins up a pipeline with a running job attached to a
// fresh agent row; the caller then flips the agent's last_seen_at / status
// to simulate crashed/zombie scenarios.
func seedRunningAgentJob(t *testing.T, pool *pgxpool.Pool) (jobID, agentID, runID uuid.UUID) {
	t.Helper()
	_, _, _, jobID, _ = seedRunningJob(t, pool)

	// seedRunningJob already inserted an `agent_id` on the compile job; fetch
	// both ids so the test can manipulate last_seen_at.
	err := pool.QueryRow(context.Background(),
		`SELECT agent_id, run_id FROM job_runs WHERE id = $1`, jobID,
	).Scan(&agentID, &runID)
	if err != nil {
		t.Fatalf("lookup job/agent: %v", err)
	}
	return
}

func TestReclaimStaleJobs_NoopWhenAgentIsHealthy(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	// Fresh last_seen_at — definitely not stale.
	_, _ = pool.Exec(ctx, `UPDATE agents SET status='online', last_seen_at=NOW() WHERE id=$1`, agentID)

	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("reclaimed %d, want 0 (agent is healthy)", len(got))
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&status)
	if status != "running" {
		t.Fatalf("job status = %q, want running", status)
	}
}

func TestReclaimStaleJobs_RequeuesWhenAgentOffline(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)
	_, _ = pool.Exec(ctx, `UPDATE agents SET status='offline' WHERE id=$1`, agentID)

	// Seed a log line so we can assert it's cleared on reclaim.
	if err := s.InsertLogLine(ctx, store.LogLine{
		JobRunID: jobID, Seq: 1, Stream: "stdout", Text: "old", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionRequeued {
		t.Fatalf("got = %+v", got)
	}
	if got[0].RunID != runID {
		t.Fatalf("result run_id mismatch: %s vs %s", got[0].RunID, runID)
	}

	var status string
	var agent *uuid.UUID
	var attempt int32
	_ = pool.QueryRow(ctx, `SELECT status, agent_id, attempt FROM job_runs WHERE id=$1`, jobID).
		Scan(&status, &agent, &attempt)
	if status != "queued" || agent != nil || attempt != 1 {
		t.Fatalf("post-reclaim row: status=%q agent=%v attempt=%d", status, agent, attempt)
	}

	var logCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM log_lines WHERE job_run_id=$1`, jobID).Scan(&logCount)
	if logCount != 0 {
		t.Fatalf("log lines = %d, want 0 (cleared on reclaim)", logCount)
	}
}

func TestReclaimStaleJobs_RequeuesWhenLastSeenIsOld(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	_, _ = pool.Exec(ctx,
		`UPDATE agents SET status='online', last_seen_at=NOW() - INTERVAL '5 minutes' WHERE id=$1`, agentID)

	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionRequeued {
		t.Fatalf("got = %+v", got)
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %q", status)
	}
}

func TestReclaimStaleJobs_FailsAtMaxAttempts(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)
	_, _ = pool.Exec(ctx, `UPDATE agents SET status='offline' WHERE id=$1`, agentID)
	// Prime attempt to the cap so the next sweep pushes it over.
	_, _ = pool.Exec(ctx, `UPDATE job_runs SET attempt = 3 WHERE id=$1`, jobID)

	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionFailed {
		t.Fatalf("got = %+v", got)
	}

	var status, errMsg string
	_ = pool.QueryRow(ctx, `SELECT status, COALESCE(error,'') FROM job_runs WHERE id=$1`, jobID).
		Scan(&status, &errMsg)
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if errMsg == "" {
		t.Fatalf("error message empty on max-attempt fail")
	}

	// Stage/run should cascade to failed too (legacy CompleteJob path).
	var runStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus)
	if runStatus != "failed" {
		t.Fatalf("run status = %q, want failed", runStatus)
	}
}

func TestReclaimStaleJobs_IgnoresNullAgent(t *testing.T) {
	// Queued jobs (no agent) must not surface as stale — they're healthy
	// from the reaper's perspective.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	_, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("reclaimed %d queued jobs, want 0", len(got))
	}
}

func TestMarkAgentSeen_UpdatesLastSeenAt(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	var agentID uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash, last_seen_at)
		 VALUES ('seen-1', 'h', NOW() - INTERVAL '1 hour') RETURNING id`,
	).Scan(&agentID)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	before := time.Now().Add(-10 * time.Second)
	if err := s.MarkAgentSeen(ctx, agentID); err != nil {
		t.Fatalf("MarkAgentSeen: %v", err)
	}
	var got time.Time
	_ = pool.QueryRow(ctx, `SELECT last_seen_at FROM agents WHERE id=$1`, agentID).Scan(&got)
	if !got.After(before) {
		t.Fatalf("last_seen_at = %v, expected recent", got)
	}
}
