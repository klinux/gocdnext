package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func logCount(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM log_lines WHERE job_run_id=$1`, jobID).Scan(&n); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	return n
}

func jobStatusAttempt(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) (string, int32) {
	t.Helper()
	var status string
	var attempt int32
	if err := pool.QueryRow(context.Background(),
		`SELECT status, attempt FROM job_runs WHERE id=$1`, jobID).Scan(&status, &attempt); err != nil {
		t.Fatalf("read job: %v", err)
	}
	return status, attempt
}

// A retry_unsafe (deploy/environment) job whose agent goes stale must be
// FAILED terminal — NOT requeued — and via the fail path that PRESERVES
// evidence (logs/artifacts/test results), unlike the requeue cleanup.
func TestReclaimStaleJobs_RetryUnsafeFailsPreservingEvidence(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE job_runs SET retry_unsafe=true WHERE id=$1`, jobID); err != nil {
		t.Fatalf("mark retry_unsafe: %v", err)
	}
	// Evidence from the attempt that's about to be lost.
	if err := s.InsertLogLine(ctx, store.LogLine{
		JobRunID: jobID, Seq: 1, Stream: "stdout", Text: "deploy step 1/3", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE agents SET status='offline', last_seen_at=NOW()-INTERVAL '10 minutes' WHERE id=$1`, agentID); err != nil {
		t.Fatalf("crash agent: %v", err)
	}

	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("reclaim results = %d, want 1", len(got))
	}
	if got[0].Action != store.ReclaimActionFailed {
		t.Fatalf("action = %q, want %q (retry_unsafe must not requeue)", got[0].Action, store.ReclaimActionFailed)
	}
	status, attempt := jobStatusAttempt(t, pool, jobID)
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if attempt != 0 {
		t.Fatalf("attempt = %d, want 0 (terminal fail must not bump attempt)", attempt)
	}
	if n := logCount(t, pool, jobID); n == 0 {
		t.Fatalf("log lines were deleted; retry_unsafe fail must PRESERVE evidence (not take the requeue cleanup)")
	}
}

// Regression guard: the retry_unsafe change must NOT alter the normal path —
// a plain (safe) stale job still requeues with attempt bumped, exactly as
// before.
func TestReclaimStaleJobs_PlainJobStillRequeues(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE agents SET status='offline', last_seen_at=NOW()-INTERVAL '10 minutes' WHERE id=$1`, agentID); err != nil {
		t.Fatalf("crash agent: %v", err)
	}

	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionRequeued {
		t.Fatalf("results = %+v, want one requeued (normal path unchanged)", got)
	}
	status, attempt := jobStatusAttempt(t, pool, jobID)
	if status != "queued" || attempt != 1 {
		t.Fatalf("status=%q attempt=%d, want queued/1", status, attempt)
	}
}

// HandleDisruptedJob is the DISRUPTED (preemption) analogue of the reaper:
// it decides requeue-vs-terminal from retry_unsafe / cancel / cap and drives
// the matching DB write. One subtest per outcome.
func TestHandleDisruptedJob_Outcomes(t *testing.T) {
	t.Run("requeued: plain job under the cap", func(t *testing.T) {
		pool := dbtest.SetupPool(t)
		s := store.New(pool)
		ctx := context.Background()
		jobID, agentID, _ := seedRunningAgentJob(t, pool)

		res, err := s.HandleDisruptedJob(ctx, store.HandleDisruptedInput{
			JobRunID: jobID, ExpectedAgentID: agentID, ExpectedAttempt: 0,
			MaxAttempts: 3, Reason: "node preemption",
		})
		if err != nil {
			t.Fatalf("HandleDisruptedJob: %v", err)
		}
		if res.Outcome != store.DisruptedRequeued {
			t.Fatalf("outcome = %q, want requeued", res.Outcome)
		}
		if status, attempt := jobStatusAttempt(t, pool, jobID); status != "queued" || attempt != 1 {
			t.Fatalf("status=%q attempt=%d, want queued/1", status, attempt)
		}
	})

	t.Run("failed_unsafe_target: deploy/env job never auto-retried", func(t *testing.T) {
		pool := dbtest.SetupPool(t)
		s := store.New(pool)
		ctx := context.Background()
		jobID, agentID, _ := seedRunningAgentJob(t, pool)
		if _, err := pool.Exec(ctx, `UPDATE job_runs SET retry_unsafe=true WHERE id=$1`, jobID); err != nil {
			t.Fatalf("mark retry_unsafe: %v", err)
		}

		res, err := s.HandleDisruptedJob(ctx, store.HandleDisruptedInput{
			JobRunID: jobID, ExpectedAgentID: agentID, ExpectedAttempt: 0,
			MaxAttempts: 3, Reason: "node preemption",
		})
		if err != nil {
			t.Fatalf("HandleDisruptedJob: %v", err)
		}
		if res.Outcome != store.DisruptedFailedUnsafe {
			t.Fatalf("outcome = %q, want failed_unsafe_target", res.Outcome)
		}
		if status, _ := jobStatusAttempt(t, pool, jobID); status != "failed" {
			t.Fatalf("status = %q, want failed", status)
		}
	})

	t.Run("failed_capped: retry cap reached", func(t *testing.T) {
		pool := dbtest.SetupPool(t)
		s := store.New(pool)
		ctx := context.Background()
		jobID, agentID, _ := seedRunningAgentJob(t, pool)
		if _, err := pool.Exec(ctx, `UPDATE job_runs SET attempt=3 WHERE id=$1`, jobID); err != nil {
			t.Fatalf("bump attempt: %v", err)
		}

		res, err := s.HandleDisruptedJob(ctx, store.HandleDisruptedInput{
			JobRunID: jobID, ExpectedAgentID: agentID, ExpectedAttempt: 3,
			MaxAttempts: 3, Reason: "node preemption",
		})
		if err != nil {
			t.Fatalf("HandleDisruptedJob: %v", err)
		}
		if res.Outcome != store.DisruptedFailedCapped {
			t.Fatalf("outcome = %q, want failed_capped", res.Outcome)
		}
		if status, _ := jobStatusAttempt(t, pool, jobID); status != "failed" {
			t.Fatalf("status = %q, want failed", status)
		}
	})

	t.Run("canceled: operator cancel won the race", func(t *testing.T) {
		pool := dbtest.SetupPool(t)
		s := store.New(pool)
		ctx := context.Background()
		jobID, agentID, _ := seedRunningAgentJob(t, pool)
		// An operator cancel landed before the disruption arrived.
		if _, err := pool.Exec(ctx, `UPDATE job_runs SET cancel_requested_at=NOW() WHERE id=$1`, jobID); err != nil {
			t.Fatalf("stamp cancel: %v", err)
		}

		res, err := s.HandleDisruptedJob(ctx, store.HandleDisruptedInput{
			JobRunID: jobID, ExpectedAgentID: agentID, ExpectedAttempt: 0,
			MaxAttempts: 3, Reason: "node preemption",
		})
		if err != nil {
			t.Fatalf("HandleDisruptedJob: %v", err)
		}
		if res.Outcome != store.DisruptedCanceled {
			t.Fatalf("outcome = %q, want canceled", res.Outcome)
		}
		// Effective status is 'canceled', never 'failed' — no hanging job,
		// no failure-metric pollution.
		if status, _ := jobStatusAttempt(t, pool, jobID); status != "canceled" {
			t.Fatalf("status = %q, want canceled", status)
		}
	})
}

// Mirror of the ReclaimStaleJobs guard for the register-fence path
// (ReclaimAgentJobs): when an agent re-registers, a retry_unsafe deploy/env
// job attributed to the prior process must also FAIL terminal (evidence
// preserved), never requeue.
func TestReclaimAgentJobs_RetryUnsafeFailsPreservingEvidence(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE job_runs SET retry_unsafe=true WHERE id=$1`, jobID); err != nil {
		t.Fatalf("mark retry_unsafe: %v", err)
	}
	if err := s.InsertLogLine(ctx, store.LogLine{
		JobRunID: jobID, Seq: 1, Stream: "stdout", Text: "deploy step 1/3", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	// Register-fence: the agent re-registered → reclaim its still-running jobs.
	got, err := s.ReclaimAgentJobs(ctx, agentID, 3)
	if err != nil {
		t.Fatalf("ReclaimAgentJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionFailed {
		t.Fatalf("results = %+v, want one failed (retry_unsafe must not requeue)", got)
	}
	status, attempt := jobStatusAttempt(t, pool, jobID)
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if attempt != 0 {
		t.Fatalf("attempt = %d, want 0 (terminal fail must not bump attempt)", attempt)
	}
	if n := logCount(t, pool, jobID); n == 0 {
		t.Fatalf("evidence deleted; retry_unsafe fence-fail must PRESERVE logs")
	}
}

// The stamping half (slice 3): a job with environment:/deploy: is created
// with retry_unsafe=TRUE, a plain job with FALSE. This exercises the same
// TargetEnvironment() predicate the upgrade backfill mirrors in SQL.
func TestCreateRun_StampsRetryUnsafeFromTargetEnvironment(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	fp := store.FingerprintFor("https://github.com/org/stamp", "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "proj-stamp", Name: "StampTest",
		Pipelines: []*domain.Pipeline{{
			Name: "p1", Stages: []string{"build", "ship"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/stamp", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "compile", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}},
				{Name: "deploy-prod", Stage: "ship", Image: "alpine", Environment: "production", Tasks: []domain.Task{{Script: "true"}}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applied.Pipelines[0].PipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "deadbeef", Branch: "main", Provider: "github", Delivery: "t", TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	unsafe := map[string]bool{}
	for _, j := range res.JobRuns {
		var ru bool
		if err := pool.QueryRow(ctx, `SELECT retry_unsafe FROM job_runs WHERE id=$1`, j.ID).Scan(&ru); err != nil {
			t.Fatalf("read retry_unsafe for %s: %v", j.Name, err)
		}
		unsafe[j.Name] = ru
	}
	if !unsafe["deploy-prod"] {
		t.Errorf("deploy-prod retry_unsafe = false, want true (has environment:)")
	}
	if unsafe["compile"] {
		t.Errorf("compile retry_unsafe = true, want false (plain job)")
	}
}
