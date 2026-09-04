package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func requirePendingTerminalEffects(t *testing.T, s *store.Store, ctx context.Context, runID uuid.UUID) {
	t.Helper()
	pending, err := s.ListPendingRunTerminalEffects(ctx, 100)
	if err != nil {
		t.Fatalf("list pending terminal effects: %v", err)
	}
	for _, id := range pending {
		if id == runID {
			return
		}
	}
	t.Fatalf("run %s missing from pending terminal effects: %v", runID, pending)
}

// #207 Part 2d: the general reaper (ReclaimStaleJobs) must NOT requeue or fail a
// cancel-requested running job — that would mask an intentional cancel as a generic
// reaper action (failed/requeued). The cancel-pending finaliser is what turns it
// into 'canceled'.
func TestReclaimStaleJobs_SkipsCancelRequested(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	// Agent offline (would normally make the job reclaimable) + a cancel stamped.
	if _, err := pool.Exec(ctx, `UPDATE agents SET status='offline' WHERE id=$1`, agentID); err != nil {
		t.Fatalf("offline: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at=NOW(), cancel_origin='user_job' WHERE id=$1`, jobID); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	// The general stale sweep must leave it running (guarded off).
	if _, err := s.ReclaimStaleJobs(ctx, 3, 0); err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, jobID); got != "running" {
		t.Fatalf("status = %q, want running (reaper must not requeue/fail a cancel-pending job)", got)
	}

	// The dedicated offline-agent cancel finaliser is what terminalises it.
	if _, err := s.ReclaimPendingCancelsForOfflineAgent(ctx, 0); err != nil {
		t.Fatalf("ReclaimPendingCancelsForOfflineAgent: %v", err)
	}
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, jobID); got != "canceled" {
		t.Errorf("status = %q, want canceled (offline-agent cancel finaliser)", got)
	}
}

// #207 Part 2d: ReclaimPendingCancelsForOfflineAgent must finalise an AGENT-owned
// deploy job's revision 'canceled' too (keyed by the returned attempt), so job +
// revision stay atomically consistent. Without this the job lands canceled while
// its revision lingers 'in_progress' / gets read as a failed deploy.
func TestReclaimPendingCancels_FinalizesAgentDeployRevision(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)
	// Agent offline + cancel stamped (agent-owned, NOT native).
	if _, err := pool.Exec(ctx, `UPDATE agents SET status='offline' WHERE id=$1`, agentID); err != nil {
		t.Fatalf("offline: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at=NOW(), cancel_origin='user_job' WHERE id=$1`, jobID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	// An in_progress deploy revision linked to the running job's (job_run, attempt).
	projectID := projectIDForRun(t, pool, runID)
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	var attempt int32
	if err := pool.QueryRow(ctx, `SELECT attempt FROM job_runs WHERE id=$1`, jobID).Scan(&attempt); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO deployment_revisions (environment_id, job_run_id, attempt, version, status)
		 VALUES ($1, $2, $3, 'v1', 'in_progress')`, envID, jobID, attempt); err != nil {
		t.Fatalf("insert revision: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='success', finished_at=NOW()
		 WHERE run_id=$1 AND id <> $2`, runID, jobID); err != nil {
		t.Fatalf("terminalise sibling jobs: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status='success', finished_at=NOW()
		 WHERE run_id=$1
		   AND id <> (SELECT stage_run_id FROM job_runs WHERE id=$2)`,
		runID, jobID); err != nil {
		t.Fatalf("terminalise sibling stages: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='running' WHERE id=$1`, runID); err != nil {
		t.Fatalf("promote run: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status='running'
		 WHERE id=(SELECT stage_run_id FROM job_runs WHERE id=$1)`, jobID); err != nil {
		t.Fatalf("promote stage: %v", err)
	}

	got, err := s.ReclaimPendingCancelsForOfflineAgent(ctx, 0)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("reclaimed %d, want 1", len(got))
	}
	if st := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, jobID); st != "canceled" {
		t.Errorf("job status = %q, want canceled", st)
	}
	if st := scalarStr(t, pool, `SELECT status FROM deployment_revisions WHERE job_run_id=$1`, jobID); st != "canceled" {
		t.Errorf("revision status = %q, want canceled (finalised by the returned attempt)", st)
	}
	if st := scalarStr(t, pool, `SELECT status FROM runs WHERE id=$1`, runID); st != "canceled" {
		t.Errorf("run status = %q, want canceled", st)
	}
	requirePendingTerminalEffects(t, s, ctx, runID)
}

// #207 Part 2e: a cancel-requested NATIVE deploy (agent_id NULL) whose deploy_watch
// vanished is finalised 'canceled' with its revision 'canceled' — but only past the
// grace window and only when NO deploy_watch row exists.
func TestReclaimAbandonedNativeCancels(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, runID := seedRunningAgentJob(t, pool)
	// Make it a server-managed native deploy (agent_id NULL) with an in_progress
	// revision linked to the job, and NO deploy_watch.
	if _, err := pool.Exec(ctx, `UPDATE job_runs SET agent_id=NULL WHERE id=$1`, jobID); err != nil {
		t.Fatalf("native: %v", err)
	}
	projectID := projectIDForRun(t, pool, runID)
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	var attempt int32
	if err := pool.QueryRow(ctx, `SELECT attempt FROM job_runs WHERE id=$1`, jobID).Scan(&attempt); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO deployment_revisions (environment_id, job_run_id, attempt, version, status)
		 VALUES ($1, $2, $3, 'v1', 'in_progress')`, envID, jobID, attempt); err != nil {
		t.Fatalf("insert revision: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='success', finished_at=NOW()
		 WHERE run_id=$1 AND id <> $2`, runID, jobID); err != nil {
		t.Fatalf("terminalise sibling jobs: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status='success', finished_at=NOW()
		 WHERE run_id=$1
		   AND id <> (SELECT stage_run_id FROM job_runs WHERE id=$2)`,
		runID, jobID); err != nil {
		t.Fatalf("terminalise sibling stages: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='running' WHERE id=$1`, runID); err != nil {
		t.Fatalf("promote run: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status='running'
		 WHERE id=(SELECT stage_run_id FROM job_runs WHERE id=$1)`, jobID); err != nil {
		t.Fatalf("promote stage: %v", err)
	}

	// (1) Fresh stamp: within grace → NOT reclaimed yet (don't race the watcher).
	if _, err := pool.Exec(ctx, `UPDATE job_runs SET cancel_requested_at=NOW(), cancel_origin='user_run' WHERE id=$1`, jobID); err != nil {
		t.Fatalf("stamp fresh: %v", err)
	}
	if n, err := s.ReclaimAbandonedNativeCancels(ctx, 5*time.Minute); err != nil {
		t.Fatalf("reclaim (fresh): %v", err)
	} else if len(n) != 0 {
		t.Fatalf("reclaimed %d within grace, want 0", len(n))
	}

	// (2) Age the stamp past grace → reclaimed: job canceled + revision canceled.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at = NOW() - INTERVAL '10 minutes' WHERE id=$1`, jobID); err != nil {
		t.Fatalf("age stamp: %v", err)
	}
	got, err := s.ReclaimAbandonedNativeCancels(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("reclaim (aged): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("reclaimed %d, want 1", len(got))
	}
	if st := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, jobID); st != "canceled" {
		t.Errorf("job status = %q, want canceled", st)
	}
	if st := scalarStr(t, pool, `SELECT status FROM deployment_revisions WHERE job_run_id=$1`, jobID); st != "canceled" {
		t.Errorf("revision status = %q, want canceled (atomic with the job)", st)
	}
	if st := scalarStr(t, pool, `SELECT status FROM runs WHERE id=$1`, runID); st != "canceled" {
		t.Errorf("run status = %q, want canceled", st)
	}
	requirePendingTerminalEffects(t, s, ctx, runID)
}

// A native cancel with a LIVE deploy_watch is NOT reclaimed — the watcher owns it.
func TestReclaimAbandonedNativeCancels_SkipsWhenWatchExists(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	if _, err := s.InsertCluster(ctx, newAuthCipher(t), store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("cluster: %v", err)
	}
	jobID, _, runID := seedRunningAgentJob(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET agent_id=NULL, cancel_requested_at = NOW() - INTERVAL '10 minutes', cancel_origin='user_run' WHERE id=$1`, jobID); err != nil {
		t.Fatalf("native+stamp: %v", err)
	}
	projectID := projectIDForRun(t, pool, runID)
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	var attempt int32
	if err := pool.QueryRow(ctx, `SELECT attempt FROM job_runs WHERE id=$1`, jobID).Scan(&attempt); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	var revID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO deployment_revisions (environment_id, job_run_id, attempt, version, status)
		 VALUES ($1, $2, $3, 'v1', 'in_progress') RETURNING id`, envID, jobID, attempt).Scan(&revID); err != nil {
		t.Fatalf("revision: %v", err)
	}
	// A LIVE watch owns the job — the reclaim's NOT EXISTS must exclude it.
	if _, err := s.CreateDeployWatch(ctx, store.DeployWatchInput{
		DeploymentRevisionID: revID, ProjectID: projectID, SyncMode: "trigger",
		Cluster: "prod-gke", Application: "checkout", Namespace: "argocd",
		ExpectedRevision: "abc123", DeadlineAt: time.Now().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}

	if n, err := s.ReclaimAbandonedNativeCancels(ctx, 5*time.Minute); err != nil {
		t.Fatalf("reclaim: %v", err)
	} else if len(n) != 0 {
		t.Fatalf("reclaimed %d, want 0 (a live deploy_watch owns the job)", len(n))
	}
	if st := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, jobID); st != "running" {
		t.Errorf("job status = %q, want running (watcher owns it)", st)
	}
}
