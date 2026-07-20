package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

// A server-managed native deploy job (no agent, old started_at — normally reaped as
// a Category-2 orphan) must be SKIPPED by the reaper while its deploy_watch is alive:
// the watcher owns it (ADR-0001).
func TestReclaimStaleJobs_SkipsJobWithActiveDeployWatch(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := newAuthCipher(t)
	s.SetAuthCipher(cipher)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	// Make the job look like a server-managed deploy: no agent, started long ago.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET agent_id=NULL, started_at=NOW()-INTERVAL '10 minutes' WHERE id=$1`, jobID); err != nil {
		t.Fatalf("orphan the job: %v", err)
	}

	// Build the revision + live watch that mark the server as owner.
	projectID := projectIDForRun(t, pool, runID)
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 0, Version: "v1", DeployedBy: "svc",
	})
	if err != nil {
		t.Fatalf("create revision: %v", err)
	}
	if _, err := s.CreateDeployWatch(ctx, store.DeployWatchInput{
		DeploymentRevisionID: revID, ProjectID: projectID, SyncMode: "trigger",
		Cluster: "prod-gke", Application: "checkout", Namespace: "argocd",
		ExpectedRevision: "v1", DeadlineAt: time.Now().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("create watch: %v", err)
	}

	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("reclaimed %d jobs, want 0 (watcher owns the deploy job)", len(got))
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&status)
	if status != "running" {
		t.Fatalf("job status = %q, want running (untouched by the reaper)", status)
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

// TestReclaimStaleJobs_ReclaimsRunningWithNullAgent covers the defensive
// secondary path: an agent_id NULL'd out (manual DB intervention, future
// regression, or partial migration) on a row that's still 'running'
// would otherwise be invisible to the reaper forever — its INNER JOIN
// to agents drops these rows silently. The LEFT-JOIN variant picks them
// up after the same staleness window applied to started_at.
//
// This is the (running, agent_id=NULL) trap issue #4 documents.
func TestReclaimStaleJobs_ReclaimsRunningWithNullAgent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, _ := seedRunningAgentJob(t, pool)
	// Simulate the trap: NULL out agent_id while leaving status='running'.
	// started_at is set well outside the staleness window so the reaper
	// commits to reclaiming this orphan.
	_, err := pool.Exec(ctx, `
		UPDATE job_runs
		SET agent_id = NULL, started_at = NOW() - INTERVAL '10 minutes'
		WHERE id = $1`, jobID)
	if err != nil {
		t.Fatalf("seed null-agent orphan: %v", err)
	}

	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionRequeued {
		t.Fatalf("got = %+v, want 1 requeue", got)
	}
	if got[0].JobRunID != jobID {
		t.Fatalf("reclaimed job_id %s, want %s", got[0].JobRunID, jobID)
	}

	var status string
	var agent *uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT status, agent_id FROM job_runs WHERE id=$1`, jobID).
		Scan(&status, &agent)
	if status != "queued" {
		t.Fatalf("post-reclaim status = %q, want queued", status)
	}
	if agent != nil {
		t.Fatalf("post-reclaim agent_id = %v, want nil", agent)
	}
}

// TestReclaimStaleJobs_LeavesFreshNullAgentAlone — counterpart: a NULL-agent
// row whose started_at is WITHIN the staleness window must not be touched.
// Otherwise a job that's in the brief window between AssignJob's UPDATE and
// the agent's first heartbeat would get yanked out from under itself.
// In practice AssignJob is atomic (status + agent_id together) so this
// state shouldn't exist via normal paths, but the reaper's behaviour must
// still be deterministic for any caller that constructs it.
func TestReclaimStaleJobs_LeavesFreshNullAgentAlone(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, _ := seedRunningAgentJob(t, pool)
	_, err := pool.Exec(ctx, `
		UPDATE job_runs SET agent_id = NULL, started_at = NOW()
		WHERE id = $1`, jobID)
	if err != nil {
		t.Fatalf("seed fresh null-agent: %v", err)
	}

	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("reclaimed %d fresh null-agent rows, want 0", len(got))
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&status)
	if status != "running" {
		t.Fatalf("status = %q, want running (untouched)", status)
	}
}

// TestReclaimAgentJobs_RequeuesAllRunningJobsForAgent locks in the
// register-fence path: when an agent re-registers, the orchestrator
// declares every running job assigned to that agent_id as stale (the
// agent process that took them is gone — registration is a singular
// event per process lifetime). This is the actual fix for the
// "agent restarts, job stuck running forever, reaper skips because
// last_seen_at is fresh" deadlock the operator hit.
func TestReclaimAgentJobs_RequeuesAllRunningJobsForAgent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)
	// Keep agent looking healthy — that's the point: the existing
	// reaper wouldn't pick this up, only the fence does.
	_, _ = pool.Exec(ctx, `UPDATE agents SET status='online', last_seen_at=NOW() WHERE id=$1`, agentID)

	if err := s.InsertLogLine(ctx, store.LogLine{
		JobRunID: jobID, Seq: 1, Stream: "stdout", Text: "from previous incarnation", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	got, err := s.ReclaimAgentJobs(ctx, agentID, 3)
	if err != nil {
		t.Fatalf("ReclaimAgentJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionRequeued {
		t.Fatalf("got = %+v, want 1 requeue", got)
	}
	if got[0].RunID != runID || got[0].JobRunID != jobID {
		t.Fatalf("reclaimed ids mismatch: %+v", got[0])
	}

	var status string
	var agent *uuid.UUID
	var attempt int32
	_ = pool.QueryRow(ctx, `SELECT status, agent_id, attempt FROM job_runs WHERE id=$1`, jobID).
		Scan(&status, &agent, &attempt)
	if status != "queued" || agent != nil || attempt != 1 {
		t.Fatalf("post-fence row: status=%q agent=%v attempt=%d", status, agent, attempt)
	}

	// Old log lines wiped (matches the existing reclaim path) so the
	// retry doesn't inherit stale output.
	var logCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM log_lines WHERE job_run_id=$1`, jobID).Scan(&logCount)
	if logCount != 0 {
		t.Fatalf("log lines remaining = %d, want 0", logCount)
	}
}

// TestReclaimAgentJobs_FailsAtMaxAttempts — fence-on-register can also push
// a job over the retry cap. Cap-exceeded path uses CompleteJob so stage/run
// cascade matches the legacy reaper failure flow.
func TestReclaimAgentJobs_FailsAtMaxAttempts(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)
	_, _ = pool.Exec(ctx, `UPDATE job_runs SET attempt = 3 WHERE id=$1`, jobID)

	got, err := s.ReclaimAgentJobs(ctx, agentID, 3)
	if err != nil {
		t.Fatalf("ReclaimAgentJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionFailed {
		t.Fatalf("got = %+v, want 1 fail", got)
	}

	var status, errMsg string
	_ = pool.QueryRow(ctx, `SELECT status, COALESCE(error,'') FROM job_runs WHERE id=$1`, jobID).
		Scan(&status, &errMsg)
	if status != "failed" || errMsg == "" {
		t.Fatalf("post-fence row: status=%q err=%q", status, errMsg)
	}
	var runStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus)
	if runStatus != "failed" {
		t.Fatalf("run cascade status = %q, want failed", runStatus)
	}
}

// TestReclaimAgentJobs_NoopOnFreshAgent — first-ever register or an agent
// that genuinely has no running jobs must produce a zero-cost noop. This is
// the hot path (every register hits it).
func TestReclaimAgentJobs_NoopOnFreshAgent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	var agentID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ('fresh-agent', 'h') RETURNING id`,
	).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	got, err := s.ReclaimAgentJobs(ctx, agentID, 3)
	if err != nil {
		t.Fatalf("ReclaimAgentJobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("reclaimed %d on fresh agent, want 0", len(got))
	}
}

// TestReclaimAgentJobs_OnlyTouchesGivenAgent guards the scope: an agent
// re-registering must not yank jobs assigned to OTHER agents. With multiple
// agents in the fleet this is the difference between "self-heal stuck
// runs" and "stampede that crashes the cluster".
func TestReclaimAgentJobs_OnlyTouchesGivenAgent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobAID, agentA, _ := seedRunningAgentJob(t, pool)
	// Spin a second running job tied to a DIFFERENT agent. Both
	// agents look healthy.
	jobBID, agentB, _ := seedRunningAgentJob(t, pool)
	_, _ = pool.Exec(ctx, `UPDATE agents SET status='online', last_seen_at=NOW() WHERE id IN ($1, $2)`, agentA, agentB)

	got, err := s.ReclaimAgentJobs(ctx, agentA, 3)
	if err != nil {
		t.Fatalf("ReclaimAgentJobs: %v", err)
	}
	if len(got) != 1 || got[0].JobRunID != jobAID {
		t.Fatalf("got = %+v, want only jobA reclaimed", got)
	}

	// jobB stays running because agentB didn't register.
	var statusB string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobBID).Scan(&statusB)
	if statusB != "running" {
		t.Fatalf("jobB status = %q, want running (untouched)", statusB)
	}
}

// TestReclaimAgentJobs_IgnoresAlreadyTerminal handles the race where the
// agent's last JobResult lands AFTER the agent's stream broke but BEFORE
// our fence fires. The job is already success/failed by the time we get
// here — touching it would corrupt the audit trail.
func TestReclaimAgentJobs_IgnoresAlreadyTerminal(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	// Race-window simulation: result landed first.
	_, _ = pool.Exec(ctx,
		`UPDATE job_runs SET status='success', finished_at=NOW(), exit_code=0 WHERE id=$1`, jobID)

	got, err := s.ReclaimAgentJobs(ctx, agentID, 3)
	if err != nil {
		t.Fatalf("ReclaimAgentJobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("reclaimed %d terminal jobs, want 0", len(got))
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&status)
	if status != "success" {
		t.Fatalf("status = %q, want success (untouched)", status)
	}
}

// TestReclaimJobForRetry_StaleSnapshotIsNoop exercises the CAS guard
// directly on the SQL: a caller that observed (agent=A0, attempt=N)
// must not be able to requeue the row after a concurrent path moved
// it to (agent=A1, attempt=N+1). This is the predicate inside
// ReclaimJobForRetry — running the higher-level Reclaim* paths
// can't reach this directly because the List step filters by the
// stale agent and returns zero. Hits the query directly via the
// store internals so the guard itself is what we cover.
//
// Reviewer caught that the earlier version of this test mutated the
// row before the List step, so neither CAS query actually ran —
// the test passed even without the snapshot predicate. This
// version goes one level deeper and proves the predicate is the
// load-bearing check.
func TestReclaimJobForRetry_StaleSnapshotIsNoop(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, originalAgent, _ := seedRunningAgentJob(t, pool)

	// Simulate the race: between our snapshot read and our reclaim,
	// a concurrent path requeued + redispatched the row to a NEW
	// agent with attempt bumped. Our caller still holds the
	// (originalAgent, attempt=0) snapshot.
	var newAgent uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash, status, last_seen_at)
		 VALUES ('redispatched-agent', 'h', 'online', NOW()) RETURNING id`,
	).Scan(&newAgent); err != nil {
		t.Fatalf("seed redispatched agent: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET agent_id=$1, attempt=attempt+1 WHERE id=$2`,
		newAgent, jobID,
	); err != nil {
		t.Fatalf("simulate redispatch: %v", err)
	}

	// Invoke the snapshot-validating CAS directly with the STALE
	// (originalAgent, attempt=0) snapshot. Predicate must refuse
	// to match — agent_id is now newAgent and attempt is 1.
	res := store.ReclaimResult{JobRunID: jobID}
	err := s.RequeueStaleJobForTest(ctx, jobID, 3,
		0 /*expectedAttempt=stale*/, originalAgent /*expectedAgent=stale*/, false, &res)
	if err != nil {
		t.Fatalf("requeueStaleJob: %v", err)
	}
	if res.Action != store.ReclaimActionSkipped {
		t.Fatalf("res.Action = %q, want Skipped (stale snapshot rejected)", res.Action)
	}

	// Row must be untouched: still running on the NEW agent at
	// attempt=1. The stale snapshot didn't clobber it.
	var status string
	var agent uuid.UUID
	var attempt int32
	if err := pool.QueryRow(ctx,
		`SELECT status, agent_id, attempt FROM job_runs WHERE id=$1`, jobID,
	).Scan(&status, &agent, &attempt); err != nil {
		t.Fatalf("post-snapshot lookup: %v", err)
	}
	if status != "running" {
		t.Fatalf("status = %q, want running (CAS protected)", status)
	}
	if agent != newAgent {
		t.Fatalf("agent_id = %s, want %s (CAS protected)", agent, newAgent)
	}
	if attempt != 1 {
		t.Fatalf("attempt = %d, want 1 (CAS protected)", attempt)
	}
}

// TestFailStaleJobAtMax_StaleSnapshotIsNoop — twin of the test above
// for the cap-exceeded path. Same race shape; the FailStaleJobAtMax
// predicate must refuse to flip status=failed when the (agent,
// attempt) snapshot is stale.
func TestFailStaleJobAtMax_StaleSnapshotIsNoop(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, originalAgent, _ := seedRunningAgentJob(t, pool)
	// Push attempt to the cap so the fail-at-max path would fire
	// if the snapshot were fresh.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET attempt=3 WHERE id=$1`, jobID,
	); err != nil {
		t.Fatalf("prime attempt: %v", err)
	}

	// Race-window: row gets requeued + redispatched + bumped to
	// attempt=4 on a different agent before our cap-exceeded path
	// runs.
	var newAgent uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash, status, last_seen_at)
		 VALUES ('redispatched-cap', 'h', 'online', NOW()) RETURNING id`,
	).Scan(&newAgent); err != nil {
		t.Fatalf("seed redispatched agent: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET agent_id=$1, attempt=attempt+1 WHERE id=$2`,
		newAgent, jobID,
	); err != nil {
		t.Fatalf("simulate redispatch: %v", err)
	}

	_, ok, err := s.FailJobIfStaleForTest(ctx, jobID, 3, originalAgent, "stale fence")
	if err != nil {
		t.Fatalf("FailJobIfStale: %v", err)
	}
	if ok {
		t.Fatal("FailStaleJobAtMax matched the stale snapshot; CAS guard is gone")
	}

	var status string
	var agent uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT status, agent_id FROM job_runs WHERE id=$1`, jobID,
	).Scan(&status, &agent); err != nil {
		t.Fatalf("post-snapshot lookup: %v", err)
	}
	if status != "running" {
		t.Fatalf("status = %q, want running (CAS protected)", status)
	}
	if agent != newAgent {
		t.Fatalf("agent_id = %s, want %s (CAS protected)", agent, newAgent)
	}
}

// TestReclaimStaleJobs_NullAgentWithNullStartedAt_ReclaimsImmediately —
// reviewer caught that the old COALESCE(started_at, NOW()) hid a class
// of corruption: status='running' AND agent_id IS NULL AND started_at
// IS NULL would never trip the staleness window because COALESCE made
// it look infinitely fresh. That state is unreachable via normal paths
// (AssignJob sets both, ReclaimJobForRetry flips both), so when it
// exists, it's data corruption and should be reclaimed on the next
// reaper tick instead of waiting forever.
func TestReclaimStaleJobs_NullAgentWithNullStartedAt_ReclaimsImmediately(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, _ := seedRunningAgentJob(t, pool)
	// The pathological combo: NULL agent, NULL started_at, running.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET agent_id = NULL, started_at = NULL WHERE id = $1`,
		jobID,
	); err != nil {
		t.Fatalf("seed corrupt orphan: %v", err)
	}

	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionRequeued {
		t.Fatalf("got = %+v, want 1 requeue (corruption pattern)", got)
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id = $1`, jobID).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %q, want queued", status)
	}
}

// TestReclaimAgentJobs_NoNotifyDuringFence locks in the
// no-notify-while-fencing contract: the fence path must NOT emit
// pg_notify on RunQueuedChannel while it's reclaiming, otherwise the
// scheduler can wake up, see the old session as idle, and re-dispatch
// the just-requeued job to it (the prior incarnation of the agent
// that the fence is fencing OFF). Asserts the fence-path requeue
// completes WITHOUT pushing into the LISTEN channel.
func TestReclaimAgentJobs_NoNotifyDuringFence(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)

	// Open a LISTENer BEFORE the fence so we don't miss the notify.
	listenConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listen conn: %v", err)
	}
	defer listenConn.Release()
	if _, err := listenConn.Exec(ctx, "LISTEN "+store.RunQueuedChannel); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	got, err := s.ReclaimAgentJobs(ctx, agentID, 3)
	if err != nil {
		t.Fatalf("ReclaimAgentJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionRequeued {
		t.Fatalf("got = %+v, want 1 requeue", got)
	}
	_ = jobID

	// Try to receive any notify with a short deadline. None should
	// arrive — the fence path passes notify=false to requeueStaleJob,
	// the test fails fast if anything got through.
	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	n, err := listenConn.Conn().WaitForNotification(waitCtx)
	if err == nil {
		t.Fatalf("unexpected NOTIFY during fence: %+v", n)
	}
}

// TestCompleteJob_RejectsStaleAgentSnapshot — the result-handler
// twin of the reclaim CAS tests. A late JobResult from a session
// whose job was reclaimed (agent_id=NULL now) or redispatched
// (different agent_id) must NOT match the CompleteJobRun predicate.
// Reviewer caught that the OLD predicate `status IN ('queued',
// 'running')` had NO agent_id check, so a stale revoked-session
// message could complete the new attempt with the old exit code.
func TestCompleteJob_RejectsStaleAgentSnapshot(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, originalAgent, _ := seedRunningAgentJob(t, pool)

	// Simulate: row was reclaimed by the register-fence. agent_id
	// is now NULL, status flipped back to queued, attempt bumped.
	// (We don't go through ReclaimAgentJobs here — direct UPDATE
	// matches the post-fence state precisely.)
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='queued', agent_id=NULL,
		 started_at=NULL, finished_at=NULL, attempt=attempt+1 WHERE id=$1`,
		jobID,
	); err != nil {
		t.Fatalf("simulate reclaim: %v", err)
	}

	// A stale result from the original agent now arrives. Passes
	// originalAgent as ExpectedAgentID. The CompleteJobRun predicate
	// must refuse: agent_id IS NULL on the row, expected is non-NULL,
	// so `IS NOT DISTINCT FROM` is false. ok=false; row stays queued.
	_, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        jobID,
		Status:          "success",
		ExitCode:        0,
		ExpectedAgentID: originalAgent,
	})
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	if ok {
		t.Fatal("stale result was accepted after reclaim — CAS guard is gone")
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %q, want queued (untouched)", status)
	}
}

// TestCompleteJob_AcceptsMatchingAgentSnapshot is the positive twin:
// when the predicate matches, the result lands as before. Sanity
// check that adding the CAS didn't break the happy path.
func TestCompleteJob_AcceptsMatchingAgentSnapshot(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	_, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        jobID,
		Status:          "success",
		ExitCode:        0,
		ExpectedAgentID: agentID,
	})
	if err != nil || !ok {
		t.Fatalf("CompleteJob: ok=%v err=%v", ok, err)
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&status)
	if status != "success" {
		t.Fatalf("status = %q, want success", status)
	}
}

// TestCompleteJob_AcceptsQueuedWithNullExpected is the third case:
// the scheduler's dispatch-time fail path (failJobWithError) flips
// a queued (agent_id=NULL) row to failed. Must pass uuid.Nil as
// ExpectedAgentID so the NULL-NULL match via IS NOT DISTINCT FROM
// works.
func TestCompleteJob_AcceptsQueuedWithNullExpected(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// Pick any queued job — they're still (queued, NULL).
	var queuedJob uuid.UUID
	for _, j := range res.JobRuns {
		queuedJob = j.ID
		break
	}

	_, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        queuedJob,
		Status:          "failed",
		ExitCode:        -1,
		ErrorMsg:        "dispatch-time fail",
		ExpectedAgentID: uuid.Nil,
	})
	if err != nil || !ok {
		t.Fatalf("dispatch-time fail: ok=%v err=%v", ok, err)
	}
}

// TestCompleteJob_RejectsStaleAttemptOnSameAgent is the regression
// cover for HIGH #1 (round 2): the agent-id CAS alone wasn't enough.
// A k8s rolling agent restart reuses the SAME agent UUID, so a stale
// result from the OLD agent process for a job that's been
// redispatched to the NEW process matches the agent_id check.
// Attempt validation closes that gap.
func TestCompleteJob_RejectsStaleAttemptOnSameAgent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)

	// Simulate the redispatch: same agent UUID, attempt bumped to 1.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET attempt=1 WHERE id=$1`, jobID,
	); err != nil {
		t.Fatalf("simulate redispatch attempt bump: %v", err)
	}

	// Stale result with attempt=0 (the OLD attempt the dead process
	// just finished). Agent UUID matches the row's agent_id. Without
	// the attempt predicate, this would erroneously complete attempt 1.
	_, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        jobID,
		Status:          "success",
		ExitCode:        0,
		ExpectedAgentID: agentID,
		ExpectedAttempt: 0, // STALE
	})
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	if ok {
		t.Fatal("stale-attempt result was accepted; attempt CAS guard missing")
	}

	var status string
	var attempt int32
	_ = pool.QueryRow(ctx,
		`SELECT status, attempt FROM job_runs WHERE id=$1`, jobID,
	).Scan(&status, &attempt)
	if status != "running" {
		t.Fatalf("status = %q, want running (CAS protected)", status)
	}
	if attempt != 1 {
		t.Fatalf("attempt = %d, want 1 (untouched)", attempt)
	}
}

// TestUnassignJob_RollsBackOnExactSnapshot covers HIGH #1: when
// the scheduler's AssignJob succeeded but DispatchAssignment failed
// (busy session queue, agent vanished in the gRPC ms), the row must
// roll back to (queued, NULL, attempt unchanged) — otherwise the
// agent's last_seen_at keeps it looking healthy to the reaper and
// the job is orphan-running forever.
func TestUnassignJob_RollsBackOnExactSnapshot(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	var attempt int32
	_ = pool.QueryRow(ctx, `SELECT attempt FROM job_runs WHERE id=$1`, jobID).Scan(&attempt)

	runID, ok, err := s.UnassignJob(ctx, jobID, agentID, attempt)
	if err != nil {
		t.Fatalf("UnassignJob: %v", err)
	}
	if !ok {
		t.Fatal("UnassignJob ok=false on exact snapshot")
	}
	if runID == uuid.Nil {
		t.Fatal("UnassignJob returned uuid.Nil run id")
	}

	var status string
	var aid *uuid.UUID
	var newAttempt int32
	_ = pool.QueryRow(ctx, `SELECT status, agent_id, attempt FROM job_runs WHERE id=$1`, jobID).
		Scan(&status, &aid, &newAttempt)
	if status != "queued" {
		t.Errorf("status = %q, want queued", status)
	}
	if aid != nil {
		t.Errorf("agent_id = %v, want NULL", aid)
	}
	if newAttempt != attempt {
		t.Errorf("attempt bumped from %d to %d; rollback must preserve", attempt, newAttempt)
	}
}

// TestUnassignJob_NoopOnStaleSnapshot — a concurrent reaper /
// rerun / fence may have moved the row to a different state
// between AssignJob and our Unassign attempt. The CAS must refuse
// to undo what's now somebody else's row.
func TestUnassignJob_NoopOnStaleSnapshot(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	// Simulate concurrent attempt-bump (a redispatch on the same row).
	_, _ = pool.Exec(ctx, `UPDATE job_runs SET attempt=attempt+1 WHERE id=$1`, jobID)

	_, ok, err := s.UnassignJob(ctx, jobID, agentID, 0 /*stale attempt*/)
	if err != nil {
		t.Fatalf("UnassignJob: %v", err)
	}
	if ok {
		t.Fatal("UnassignJob matched the stale snapshot — CAS guard missing")
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&status)
	if status != "running" {
		t.Errorf("status = %q after stale Unassign, want running (untouched)", status)
	}
}

// TestMarkAgentOffline_NoopWhenSessionSuperseded — HIGH #2's
// regression cover. agents.session_generation is the CAS key the
// Connect handler's defer carries; MarkAgentOffline must no-op
// when a successor MarkAgentOnline has bumped the counter past
// the value the closing defer observed.
//
// Why generation instead of session UUID: persisting the session
// id in the DB would leak a bearer credential through any
// read-only DB exposure (backup, snapshot, log). A monotonic int
// carries the "is this defer's epoch current?" signal with no
// authentication power — see migration 00033.
func TestMarkAgentOffline_NoopWhenSessionSuperseded(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	var agentID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ('mao-test', 'h') RETURNING id`,
	).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Old session → online; capture generation.
	oldGen, err := s.MarkAgentOnline(ctx, agentID, store.RegisterUpdate{})
	if err != nil {
		t.Fatalf("MarkAgentOnline old: %v", err)
	}
	// Successor session → online (bumps generation).
	newGen, err := s.MarkAgentOnline(ctx, agentID, store.RegisterUpdate{})
	if err != nil {
		t.Fatalf("MarkAgentOnline new: %v", err)
	}
	if newGen <= oldGen {
		t.Fatalf("generation didn't bump: old=%d new=%d", oldGen, newGen)
	}

	// Old stream's defer eventually fires, carrying oldGen. Row's
	// session_generation is newGen by now → CAS mismatch → no-op.
	if err := s.MarkAgentOffline(ctx, agentID, oldGen); err != nil {
		t.Fatalf("MarkAgentOffline old: %v", err)
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM agents WHERE id=$1`, agentID).Scan(&status)
	if status != "online" {
		t.Fatalf("status = %q, want online (CAS protected the successor)", status)
	}
}

// TestMarkAgentOffline_FiresWhenSessionStillCurrent — twin: when
// the defer's observed generation IS the row's current generation,
// the offline mark goes through (normal disconnect path).
func TestMarkAgentOffline_FiresWhenSessionStillCurrent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	var agentID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ('mao-fires', 'h') RETURNING id`,
	).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	gen, err := s.MarkAgentOnline(ctx, agentID, store.RegisterUpdate{})
	if err != nil {
		t.Fatalf("MarkAgentOnline: %v", err)
	}
	if err := s.MarkAgentOffline(ctx, agentID, gen); err != nil {
		t.Fatalf("MarkAgentOffline: %v", err)
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM agents WHERE id=$1`, agentID).Scan(&status)
	if status != "offline" {
		t.Fatalf("status = %q, want offline (normal disconnect)", status)
	}
}

// TestWriteTestResults_RejectsStaleSnapshot — MED regression cover:
// the test results CAS must reject a batch whose (agent, attempt)
// snapshot no longer matches the row. Without it, a session that
// outlives a reaper-driven reclaim could silently clobber the new
// attempt's test_results via the delete+insert pattern.
func TestWriteTestResults_RejectsStaleSnapshot(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	// Caller observes attempt=0, agent=originalAgent. Concurrent
	// reaper requeues + redispatches: attempt=1, different agent.
	var newAgent uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash, status, last_seen_at)
		 VALUES ('tr-new', 'h', 'online', NOW()) RETURNING id`,
	).Scan(&newAgent); err != nil {
		t.Fatalf("seed new agent: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET agent_id=$1, attempt=attempt+1 WHERE id=$2`,
		newAgent, jobID,
	); err != nil {
		t.Fatalf("simulate redispatch: %v", err)
	}

	err := s.WriteTestResults(ctx, jobID, agentID, 0,
		[]store.TestResultIn{{Suite: "s", Name: "t", Status: store.TestStatusPassed}})
	if !errors.Is(err, store.ErrSnapshotStale) {
		t.Fatalf("err = %v, want ErrSnapshotStale", err)
	}

	// Row's test_results count should be 0 (no clobber).
	var count int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM test_results WHERE job_run_id=$1`, jobID,
	).Scan(&count)
	if count != 0 {
		t.Fatalf("test_results count = %d, want 0 (stale write blocked)", count)
	}
}

// TestWriteTestResults_AcceptsMatchingSnapshot is the happy path:
// the (agent, attempt) the caller observed is what's on the row,
// the delete+insert lands cleanly.
func TestWriteTestResults_AcceptsMatchingSnapshot(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	err := s.WriteTestResults(ctx, jobID, agentID, 0,
		[]store.TestResultIn{
			{Suite: "s", Name: "t1", Status: store.TestStatusPassed},
			{Suite: "s", Name: "t2", Status: store.TestStatusFailed},
		})
	if err != nil {
		t.Fatalf("WriteTestResults: %v", err)
	}

	var count int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM test_results WHERE job_run_id=$1`, jobID,
	).Scan(&count)
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
}

// TestReclaim_WipesTestResults — the reaper's requeue path now
// deletes test_results alongside log_lines so a retry doesn't
// inherit the prior attempt's JUnit display. Without this the
// Tests tab would show stale rows on the new attempt until the
// agent emits a fresh report (which it may not — the rerun could
// fail before test stages run).
func TestReclaim_WipesTestResults(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)
	// Seed test_results that the reclaim should wipe.
	if err := s.WriteTestResults(ctx, jobID, agentID, 0,
		[]store.TestResultIn{{Suite: "old", Name: "t1", Status: store.TestStatusPassed}}); err != nil {
		t.Fatalf("seed test results: %v", err)
	}

	// Reap via the agent-offline path.
	_, _ = pool.Exec(ctx, `UPDATE agents SET status='offline' WHERE id=$1`, agentID)
	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionRequeued {
		t.Fatalf("expected 1 requeue, got %+v", got)
	}

	var count int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM test_results WHERE job_run_id=$1`, jobID,
	).Scan(&count)
	if count != 0 {
		t.Fatalf("test_results count = %d, want 0 (reclaim should wipe)", count)
	}
}

// TestReclaim_RetiresArtifacts is the round-1 issue-#3 regression
// test. Before the fix, requeueStaleJob deleted log_lines and
// test_results but left ready artifacts intact — so a job that
// completed once, got reclaimed by the reaper/fence, and ran
// again would end up with TWO ready rows for the same path. The
// downstream consumer's ListReadyArtifactsByRunAndJob would then
// see both (or, in the lookup-race window between the two attempts,
// see the stale ready row pointing at a storage object about to
// be swept). Mirrors TestReclaim_WipesTestResults exactly — same
// reclaim path, different table.
func TestReclaim_RetiresArtifacts(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)

	// Look up pipeline + project ids — InsertPendingArtifact needs them.
	var pipelineID, projectID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT r.pipeline_id, p.project_id
		   FROM runs r JOIN pipelines p ON p.id = r.pipeline_id
		  WHERE r.id = $1`, runID,
	).Scan(&pipelineID, &projectID); err != nil {
		t.Fatalf("lookup ids: %v", err)
	}

	// Seed two artifacts on the running attempt — simulate the prior
	// upload that issue #3 says should be retired on reclaim.
	expires := time.Now().Add(24 * time.Hour)
	for _, p := range []string{"dist/", "logs/coverage.xml"} {
		if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
			RunID:      runID,
			JobRunID:   jobID,
			PipelineID: pipelineID,
			ProjectID:  projectID,
			Path:       p,
			StorageKey: uuid.NewString(),
			ExpiresAt:  &expires,
		}); err != nil {
			t.Fatalf("seed artifact %q: %v", p, err)
		}
	}

	// Reap via agent-offline path.
	_, _ = pool.Exec(ctx, `UPDATE agents SET status='offline' WHERE id=$1`, agentID)
	got, err := s.ReclaimStaleJobs(ctx, 3, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if len(got) != 1 || got[0].Action != store.ReclaimActionRequeued {
		t.Fatalf("expected 1 requeue, got %+v", got)
	}

	// All prior artifacts must be soft-deleted (deleted_at set,
	// status flipped to 'deleting') so the new attempt can re-upload
	// the same paths without the partial-unique-index (migration
	// 00035) rejecting it, AND so ListReadyArtifactsByRunAndJob
	// no longer surfaces the stale rows to a downstream consumer.
	var active int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM artifacts WHERE job_run_id=$1 AND deleted_at IS NULL`, jobID,
	).Scan(&active)
	if active != 0 {
		t.Fatalf("active artifacts after reclaim = %d, want 0 (all should be retired)", active)
	}

	var deleting int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM artifacts WHERE job_run_id=$1 AND status='deleting' AND deleted_at IS NOT NULL`, jobID,
	).Scan(&deleting)
	if deleting != 2 {
		t.Fatalf("retired artifacts = %d, want 2", deleting)
	}
}

// TestArtifact_PathNormalization — `dist/` and `dist` collapse to
// the same canonical form on insert AND on lookup, so producer and
// consumer YAMLs can legitimately disagree on the trailing slash.
// Without normalization, the user's repro path (`packages/types/src/generated/`
// in both places) would match by luck, but any divergence — common
// when one job is hand-edited — would 0-row at dispatch time and
// surface the misleading "no ready artefacts" error.
func TestArtifact_PathNormalization(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, runID := seedRunningAgentJob(t, pool)
	var pipelineID, projectID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT r.pipeline_id, p.project_id
		   FROM runs r JOIN pipelines p ON p.id = r.pipeline_id
		  WHERE r.id = $1`, runID,
	).Scan(&pipelineID, &projectID); err != nil {
		t.Fatalf("lookup ids: %v", err)
	}

	// Insert with trailing slash; mark ready so the lookup can see it.
	key := uuid.NewString()
	if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID:      runID,
		JobRunID:   jobID,
		PipelineID: pipelineID,
		ProjectID:  projectID,
		Path:       "dist/",
		StorageKey: key,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.MarkArtifactReady(ctx, key, 100, "deadbeef"); err != nil {
		t.Fatalf("mark ready: %v", err)
	}

	// Stored canonical form is without the trailing slash.
	var stored string
	_ = pool.QueryRow(ctx,
		`SELECT path FROM artifacts WHERE storage_key=$1`, key,
	).Scan(&stored)
	if stored != "dist" {
		t.Fatalf("stored path = %q, want %q", stored, "dist")
	}

	// Look up with the trailing slash → finds it (consumer wrote it that way).
	got, err := s.ListReadyArtifactsByRunAndJob(ctx, runID, "compile", []string{"dist/"})
	if err != nil {
		t.Fatalf("lookup slash: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("lookup with trailing slash got %d, want 1", len(got))
	}

	// Look up without the slash → also finds it (consumer wrote a different way).
	got, err = s.ListReadyArtifactsByRunAndJob(ctx, runID, "compile", []string{"dist"})
	if err != nil {
		t.Fatalf("lookup no slash: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("lookup without trailing slash got %d, want 1", len(got))
	}
}

// TestArtifact_PartialUniqueIndex_BlocksDuplicateActive locks in the
// migration 00035 invariant — two active rows for the same
// (job_run_id, path) are NOT allowed. The retire path soft-deletes
// (sets deleted_at) so subsequent inserts on retry are fine, but a
// raw double-insert without retirement must fail loudly so a future
// regression in requeueStaleJob/RerunJob doesn't silently produce
// the duplicate-row bug again.
func TestArtifact_PartialUniqueIndex_BlocksDuplicateActive(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, runID := seedRunningAgentJob(t, pool)
	var pipelineID, projectID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT r.pipeline_id, p.project_id
		   FROM runs r JOIN pipelines p ON p.id = r.pipeline_id
		  WHERE r.id = $1`, runID,
	).Scan(&pipelineID, &projectID); err != nil {
		t.Fatalf("lookup ids: %v", err)
	}

	if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID:      runID,
		JobRunID:   jobID,
		PipelineID: pipelineID,
		ProjectID:  projectID,
		Path:       "dist",
		StorageKey: uuid.NewString(),
	}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Second insert with the same (job_run_id, path) — must fail.
	if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID:      runID,
		JobRunID:   jobID,
		PipelineID: pipelineID,
		ProjectID:  projectID,
		Path:       "dist",
		StorageKey: uuid.NewString(),
	}); err == nil {
		t.Fatal("expected unique-index violation on duplicate active row, got nil")
	}
}

// TestRerunJob_RetiresArtifacts — the manual-rerun path needs to
// retire the prior attempt's artifacts the same way the reaper's
// requeue does, so a rerun doesn't run into the partial-unique-
// index on its re-upload.
//
// RerunJob is normally exercised through the HTTP handler tests in
// api/runs, but those don't seed artifacts. Driving it directly
// here keeps the store-level invariant under regression coverage
// without dragging an HTTP harness into the store package.
func TestRerunJob_RetiresArtifacts(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, runID := seedRunningAgentJob(t, pool)

	// Flip the run to terminal first — RerunJob refuses queued/
	// running rows. Marking the job 'success' is enough for the
	// stage cascade to land but the easier route is to flip jobs
	// directly through the DB.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='success', finished_at=NOW() WHERE id=$1`, jobID,
	); err != nil {
		t.Fatalf("flip terminal: %v", err)
	}

	var pipelineID, projectID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT r.pipeline_id, p.project_id
		   FROM runs r JOIN pipelines p ON p.id = r.pipeline_id
		  WHERE r.id = $1`, runID,
	).Scan(&pipelineID, &projectID); err != nil {
		t.Fatalf("lookup ids: %v", err)
	}

	// Seed prior-attempt artifacts so the rerun's retire has something
	// to act on.
	expires := time.Now().Add(24 * time.Hour)
	for _, p := range []string{"dist", "logs/coverage.xml"} {
		if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
			RunID: runID, JobRunID: jobID,
			PipelineID: pipelineID, ProjectID: projectID,
			Path: p, StorageKey: uuid.NewString(), ExpiresAt: &expires,
		}); err != nil {
			t.Fatalf("seed artifact %q: %v", p, err)
		}
	}

	if _, err := s.RerunJob(ctx, store.RerunJobInput{JobRunID: jobID, TriggeredBy: "user:test"}); err != nil {
		t.Fatalf("RerunJob: %v", err)
	}

	var active int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM artifacts WHERE job_run_id=$1 AND deleted_at IS NULL`, jobID,
	).Scan(&active)
	if active != 0 {
		t.Fatalf("active artifacts after rerun = %d, want 0", active)
	}
}

// TestRerunJob_ClearsLogArchivePointers — a previous attempt may
// have shipped its log_lines to the artifact store and stamped
// logs_archive_uri + logs_archived_at on the job_run. RerunJob
// DELETEs the log_lines but unless it also clears those pointers,
// the read path's cold-archive fallback (reads.go GetRunDetail)
// will serve the PRIOR attempt's archived logs back to the UI —
// the rerun shows "logs of the previous job" until the archiver
// runs again for the new attempt and overwrites the URI.
func TestRerunJob_ClearsLogArchivePointers(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, _ := seedRunningAgentJob(t, pool)

	// Simulate the prior attempt: terminal + archived.
	archivedAt := time.Now().Add(-1 * time.Hour)
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs
		    SET status='success', finished_at=NOW(),
		        logs_archive_uri='gs://logs/prev-attempt.gz',
		        logs_archived_at=$2
		  WHERE id=$1`, jobID, archivedAt); err != nil {
		t.Fatalf("seed archived terminal row: %v", err)
	}

	if _, err := s.RerunJob(ctx, store.RerunJobInput{JobRunID: jobID, TriggeredBy: "user:test"}); err != nil {
		t.Fatalf("RerunJob: %v", err)
	}

	var status string
	var archiveURI *string
	var archivedAtAfter *time.Time
	_ = pool.QueryRow(ctx,
		`SELECT status, logs_archive_uri, logs_archived_at FROM job_runs WHERE id=$1`, jobID,
	).Scan(&status, &archiveURI, &archivedAtAfter)
	if status != "queued" {
		t.Errorf("status = %q, want queued", status)
	}
	if archiveURI != nil {
		t.Errorf("logs_archive_uri = %q, want NULL — rerun must clear stale archive pointer", *archiveURI)
	}
	if archivedAtAfter != nil {
		t.Errorf("logs_archived_at = %v, want NULL", archivedAtAfter)
	}
}

// TestRerunJob_ClearsCancelRequestedAt — a row that was canceled
// (deferred path or reaper-finalised) carries cancel_requested_at
// into its terminal state for the audit trail. When the operator
// reruns that row, the new attempt MUST NOT inherit the prior
// attempt's cancel intent — otherwise the agent's next Register
// would replay the stale cancel via ListPendingCancelsForAgent
// and kill the new attempt before it had a chance.
func TestRerunJob_ClearsCancelRequestedAt(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, _ := seedRunningAgentJob(t, pool)

	// Simulate a deferred cancel that the reaper finalised: row
	// went canceled with cancel_requested_at preserved in the
	// audit trail.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs
		    SET status='canceled', finished_at=NOW(), cancel_requested_at=NOW() - INTERVAL '1 minute'
		  WHERE id=$1`, jobID); err != nil {
		t.Fatalf("flip canceled with stamp: %v", err)
	}

	if _, err := s.RerunJob(ctx, store.RerunJobInput{JobRunID: jobID, TriggeredBy: "user:test"}); err != nil {
		t.Fatalf("RerunJob: %v", err)
	}

	var status string
	var cancelRequestedAt *time.Time
	var attempt int32
	_ = pool.QueryRow(ctx,
		`SELECT status, cancel_requested_at, attempt FROM job_runs WHERE id=$1`, jobID,
	).Scan(&status, &cancelRequestedAt, &attempt)
	if status != "queued" {
		t.Errorf("status = %q, want queued", status)
	}
	if cancelRequestedAt != nil {
		t.Errorf("cancel_requested_at = %v, want NULL — rerun must clear stale cancel intent", cancelRequestedAt)
	}
	if attempt != 1 {
		t.Errorf("attempt = %d, want 1 (bumped from 0)", attempt)
	}
}

// TestRerunJob_StampsDeployRollbackFlag pins the channel that carries
// a rollback's intent from the endpoint to the scheduler (#39 phase
// 3): a rerun marked IsRollback stamps deploy_rollback on the row, an
// ordinary rerun clears it, and ListDispatchableJobs surfaces it so
// the dispatch can open the revision with is_rollback=true.
func TestRerunJob_StampsDeployRollbackFlag(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, runID := seedRunningAgentJob(t, pool)
	mustMarkJobTerminal(t, pool, jobID)

	// Rollback rerun stamps the flag.
	if _, err := s.RerunJob(ctx, store.RerunJobInput{
		JobRunID: jobID, TriggeredBy: "user:alice", IsRollback: true,
	}); err != nil {
		t.Fatalf("RerunJob (rollback): %v", err)
	}
	if got := deployRollbackOf(t, s, runID, jobID); !got {
		t.Fatal("deploy_rollback not set after a rollback rerun")
	}

	// An ordinary rerun must CLEAR it (the row is reused; a stale true
	// would mislabel the next dispatch).
	mustMarkJobTerminal(t, pool, jobID)
	if _, err := s.RerunJob(ctx, store.RerunJobInput{
		JobRunID: jobID, TriggeredBy: "user:alice", IsRollback: false,
	}); err != nil {
		t.Fatalf("RerunJob (normal): %v", err)
	}
	if got := deployRollbackOf(t, s, runID, jobID); got {
		t.Fatal("deploy_rollback survived an ordinary rerun — stale flag")
	}
}

// TestRerunJob_ConcurrentIsSerialized is the regression for the
// check-then-update race (review HIGH): N callers rerun the SAME
// terminal job at once. The FOR UPDATE row lock serializes them, so
// EXACTLY ONE wins (job → queued) and the rest see the now-queued row
// and bail with ErrJobRunActive — the attempt is bumped once, never N
// times, and a running redispatch is never reset back to queued.
func TestRerunJob_ConcurrentIsSerialized(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, _ := seedRunningAgentJob(t, pool)
	mustMarkJobTerminal(t, pool, jobID)

	const n = 8
	var wg sync.WaitGroup
	var okCount, activeCount atomic.Int32
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := s.RerunJob(ctx, store.RerunJobInput{JobRunID: jobID, TriggeredBy: "u"})
			switch {
			case err == nil:
				okCount.Add(1)
			case errors.Is(err, store.ErrJobRunActive):
				activeCount.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if okCount.Load() != 1 {
		t.Fatalf("concurrent reruns succeeded %d times, want exactly 1", okCount.Load())
	}
	if activeCount.Load() != n-1 {
		t.Fatalf("got %d ErrJobRunActive, want %d", activeCount.Load(), n-1)
	}
	var attempt int32
	_ = pool.QueryRow(ctx, `SELECT attempt FROM job_runs WHERE id=$1`, jobID).Scan(&attempt)
	if attempt != 1 {
		t.Fatalf("attempt = %d, want 1 (bumped once despite %d concurrent reruns)", attempt, n)
	}
}

func mustMarkJobTerminal(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE job_runs SET status='success', finished_at=NOW(), agent_id=NULL WHERE id=$1`, jobID); err != nil {
		t.Fatalf("mark job terminal: %v", err)
	}
}

// deployRollbackOf reads the flag the way the scheduler does — through
// ListDispatchableJobs — so the test also covers the query plumbing.
func deployRollbackOf(t *testing.T, s *store.Store, runID, jobID uuid.UUID) bool {
	t.Helper()
	jobs, err := s.ListDispatchableJobs(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListDispatchableJobs: %v", err)
	}
	for _, j := range jobs {
		if j.ID == jobID {
			return j.DeployRollback
		}
	}
	t.Fatalf("job %s not dispatchable after rerun", jobID)
	return false
}

// TestRetireArtifactsByJobRun_ClearsPinned — pinned artifacts that
// get retired (because the owning attempt died) must have pinned_at
// cleared, otherwise the sweeper's pinned-skip guard would leave
// the storage object orphan forever.
func TestRetireArtifactsByJobRun_ClearsPinned(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, runID := seedRunningAgentJob(t, pool)
	var pipelineID, projectID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT r.pipeline_id, p.project_id
		   FROM runs r JOIN pipelines p ON p.id = r.pipeline_id
		  WHERE r.id = $1`, runID,
	).Scan(&pipelineID, &projectID); err != nil {
		t.Fatalf("lookup ids: %v", err)
	}

	// Seed a pinned artifact, then retire.
	if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID: runID, JobRunID: jobID,
		PipelineID: pipelineID, ProjectID: projectID,
		Path: "dist", StorageKey: uuid.NewString(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE artifacts SET pinned_at=NOW() WHERE job_run_id=$1`, jobID,
	); err != nil {
		t.Fatalf("pin: %v", err)
	}

	if err := s.RetireArtifactsByJobRun(ctx, jobID); err != nil {
		t.Fatalf("RetireArtifactsByJobRun: %v", err)
	}

	var pinned *time.Time
	_ = pool.QueryRow(ctx,
		`SELECT pinned_at FROM artifacts WHERE job_run_id=$1`, jobID,
	).Scan(&pinned)
	if pinned != nil {
		t.Fatalf("pinned_at = %v, want NULL (retire must clear pinning)", *pinned)
	}
}

// TestClaimArtifactsForSweep_ReapsStalePending — the new sweep
// branch must claim pending rows older than the grace window. Guards
// the orphan that the migration-00035 unique index would otherwise
// turn into a permanent "can't re-upload this path" footgun.
func TestClaimArtifactsForSweep_ReapsStalePending(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, runID := seedRunningAgentJob(t, pool)
	var pipelineID, projectID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT r.pipeline_id, p.project_id
		   FROM runs r JOIN pipelines p ON p.id = r.pipeline_id
		  WHERE r.id = $1`, runID,
	).Scan(&pipelineID, &projectID); err != nil {
		t.Fatalf("lookup ids: %v", err)
	}

	if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID: runID, JobRunID: jobID,
		PipelineID: pipelineID, ProjectID: projectID,
		Path: "dist", StorageKey: uuid.NewString(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Backdate created_at past the grace window.
	if _, err := pool.Exec(ctx,
		`UPDATE artifacts SET created_at = NOW() - INTERVAL '2 hours'
		  WHERE job_run_id=$1`, jobID,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Grace 60min; the row is 2h old → must be claimed.
	claimed, err := s.ClaimArtifactsForSweep(ctx, 10, 60)
	if err != nil {
		t.Fatalf("ClaimArtifactsForSweep: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %d, want 1 (stale pending must be reaped)", len(claimed))
	}

	var status string
	_ = pool.QueryRow(ctx,
		`SELECT status FROM artifacts WHERE job_run_id=$1`, jobID,
	).Scan(&status)
	if status != "deleting" {
		t.Fatalf("status = %q, want deleting", status)
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

// TestReclaimPendingCancelsForOfflineAgent_FinalisesWhenAgentGone —
// the reaper's path for cancels that the in-process replay couldn't
// land. Stamps cancel_requested_at on a running job, flips its
// agent offline + sets last_seen_at past the grace window, and
// asserts the reaper finalises the row to status='canceled'.
func TestReclaimPendingCancelsForOfflineAgent_FinalisesWhenAgentGone(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)

	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at = NOW() WHERE id = $1`, jobID); err != nil {
		t.Fatalf("stamp cancel_requested_at: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE agents SET status = 'offline', last_seen_at = NOW() - INTERVAL '10 minutes'
		 WHERE id = $1`, agentID); err != nil {
		t.Fatalf("flip agent offline: %v", err)
	}

	got, err := s.ReclaimPendingCancelsForOfflineAgent(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReclaimPendingCancelsForOfflineAgent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d finalised rows, want 1", len(got))
	}
	if got[0].JobRunID != jobID || got[0].RunID != runID || got[0].AgentID != agentID {
		t.Errorf("result mismatch: %+v", got[0])
	}

	var status string
	var finishedAt *time.Time
	_ = pool.QueryRow(ctx,
		`SELECT status, finished_at FROM job_runs WHERE id = $1`, jobID).
		Scan(&status, &finishedAt)
	if status != "canceled" {
		t.Errorf("post-sweep status = %q, want canceled", status)
	}
	if finishedAt == nil {
		t.Errorf("finished_at must be set after finalisation")
	}
}

// TestReclaimPendingCancelsForOfflineAgent_SkipsOnlineAgent — when
// the owning agent is online AND heartbeat is fresh (the replay
// path is still expected to land the cancel on its next Connect),
// the reaper MUST NOT finalise the row. Otherwise the reaper races
// against an in-flight replay and double-completes the row.
func TestReclaimPendingCancelsForOfflineAgent_SkipsOnlineAgent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)

	// Stamp the cancel intent + ensure the agent is fully healthy
	// (online status AND fresh last_seen_at). seedRunningAgentJob
	// leaves last_seen_at NULL which the predicate treats as stale.
	if _, err := pool.Exec(ctx,
		`UPDATE agents SET status='online', last_seen_at=NOW() WHERE id=$1`, agentID); err != nil {
		t.Fatalf("flip agent healthy: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at = NOW() WHERE id = $1`, jobID); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	got, err := s.ReclaimPendingCancelsForOfflineAgent(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReclaimPendingCancelsForOfflineAgent: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d finalised rows, want 0 (agent is online — replay path owns this)", len(got))
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id = $1`, jobID).Scan(&status)
	if status != "running" {
		t.Errorf("post-sweep status = %q, want still running (reaper must not finalise an online agent's row)", status)
	}
}

// TestReclaimPendingCancelsForOfflineAgent_FinalisesStaleHeartbeatOnline
// — an agent that's still marked online but whose heartbeat hasn't
// landed past the grace window is dead from the cancel-replay
// perspective. Predicate mirrors the reaper's main path so a
// network-partitioned agent doesn't leave a pending cancel
// hanging forever.
func TestReclaimPendingCancelsForOfflineAgent_FinalisesStaleHeartbeatOnline(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)

	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at = NOW() WHERE id = $1`, jobID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	// Agent looks online but its heartbeat is 10 minutes old.
	if _, err := pool.Exec(ctx,
		`UPDATE agents SET status='online', last_seen_at = NOW() - INTERVAL '10 minutes'
		 WHERE id = $1`, agentID); err != nil {
		t.Fatalf("flip agent online but heartbeat stale: %v", err)
	}

	got, err := s.ReclaimPendingCancelsForOfflineAgent(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReclaimPendingCancelsForOfflineAgent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d finalised rows, want 1 — stale heartbeat must count as unreachable", len(got))
	}

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id = $1`, jobID).Scan(&status)
	if status != "canceled" {
		t.Errorf("post-sweep status = %q, want canceled", status)
	}
}

// TestReclaimPendingCancelsForOfflineAgent_CascadesToStageAndRun
// — when the finalised row is the last unfinished job under its
// stage, the cascade must terminalise stage_runs.status (and the
// run, when its last stage closes). Without the cascade the job
// flips canceled but the parent stays running forever, blocking
// the operator's view of "is this run done?".
//
// seedRunningJob lays out two stages (build/compile, test/unit)
// with one job each. We terminalise every OTHER job + stage so
// the cancel-finalisation of `compile` is the trigger that closes
// its stage; the run still has the `test` stage open, so we also
// terminalise that branch to drive the run-level cascade.
func TestReclaimPendingCancelsForOfflineAgent_CascadesToStageAndRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, runID := seedRunningAgentJob(t, pool)

	// Mark every sibling job as already terminal AND every sibling
	// stage as success so the canceled job IS the trigger that
	// closes its stage AND the run.
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
	// Promote the run + the target stage to running so the cascade's
	// CompleteStageRun / CompleteRun predicates (which gate on
	// non-terminal source state) actually fire.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='running' WHERE id=$1`, runID); err != nil {
		t.Fatalf("promote run: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status='running'
		 WHERE id=(SELECT stage_run_id FROM job_runs WHERE id=$1)`, jobID); err != nil {
		t.Fatalf("promote stage: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at=NOW() WHERE id=$1`, jobID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE agents SET status='offline', last_seen_at=NOW() - INTERVAL '10 minutes'
		 WHERE id=$1`, agentID); err != nil {
		t.Fatalf("flip agent offline: %v", err)
	}

	if _, err := s.ReclaimPendingCancelsForOfflineAgent(ctx, 5*time.Minute); err != nil {
		t.Fatalf("ReclaimPendingCancelsForOfflineAgent: %v", err)
	}

	var jobStatus, stageStatus, runStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&jobStatus)
	_ = pool.QueryRow(ctx,
		`SELECT sr.status FROM stage_runs sr JOIN job_runs jr ON jr.stage_run_id = sr.id WHERE jr.id=$1`,
		jobID).Scan(&stageStatus)
	_ = pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus)

	if jobStatus != "canceled" {
		t.Errorf("job status = %q, want canceled", jobStatus)
	}
	if stageStatus == "running" || stageStatus == "queued" {
		t.Errorf("stage status = %q, want terminal (cascade missed)", stageStatus)
	}
	if runStatus == "running" || runStatus == "queued" {
		t.Errorf("run status = %q, want terminal (cascade missed)", runStatus)
	}
}

// TestReclaimPendingCancelsForOfflineAgent_RespectsGrace — the
// status='offline' branch finalises immediately (the agent told us
// it's gone); the heartbeat-stale branch waits for the grace
// window. This test pins the heartbeat-stale branch: agent stays
// online, heartbeat 30s old, grace 5min → skip.
func TestReclaimPendingCancelsForOfflineAgent_RespectsGrace(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)

	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at = NOW() WHERE id = $1`, jobID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	// Agent online, heartbeat 30s old — well inside the grace window.
	if _, err := pool.Exec(ctx,
		`UPDATE agents SET status='online', last_seen_at = NOW() - INTERVAL '30 seconds'
		 WHERE id = $1`, agentID); err != nil {
		t.Fatalf("set fresh-online heartbeat: %v", err)
	}

	got, err := s.ReclaimPendingCancelsForOfflineAgent(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReclaimPendingCancelsForOfflineAgent: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d finalised rows, want 0 — heartbeat is within the grace window", len(got))
	}
}

// TestReclaimAgentJobs_SkipsPendingCancels — the register-fence
// reclaim MUST NOT requeue/fail-at-max a row that already has
// cancel_requested_at stamped. Such rows belong to the Register
// replay path (ListPendingCancelsForAgent) which honors them as
// 'canceled' instead of dropping them back to 'queued' (where a
// subsequent dispatch would mis-attribute the operator's intent
// as a generic retry).
func TestReclaimAgentJobs_SkipsPendingCancels(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, agentID, _ := seedRunningAgentJob(t, pool)

	// Stamp the cancel intent BEFORE the register-fence kicks in.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at=NOW() WHERE id=$1`, jobID); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	got, err := s.ReclaimAgentJobs(ctx, agentID, 3)
	if err != nil {
		t.Fatalf("ReclaimAgentJobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d reclaim results, want 0 — pending cancels belong to the replay path", len(got))
	}

	// Critical: the row is STILL running with cancel_requested_at.
	// The replay path picks it up; if the reclaim had grabbed it,
	// the row would be queued (agent_id NULL) and the cancel intent
	// would be invisible to ListPendingCancelsForAgent's predicate.
	var status string
	var cancelRequestedAt *time.Time
	var agent *uuid.UUID
	_ = pool.QueryRow(ctx,
		`SELECT status, cancel_requested_at, agent_id FROM job_runs WHERE id=$1`, jobID).
		Scan(&status, &cancelRequestedAt, &agent)
	if status != "running" || agent == nil || cancelRequestedAt == nil {
		t.Errorf("row drifted: status=%q agent=%v stamp=%v — want still running + agent set + stamp set",
			status, agent, cancelRequestedAt)
	}
}

// TestListPendingCancelsForAgent_ReturnsOnlyOwnedAndPending — the
// agent's replay query MUST scope to rows belonging to this agent
// AND with cancel_requested_at stamped. A canceled-but-no-stamp
// row, or a stamp on another agent's row, must NOT leak into this
// agent's replay set.
func TestListPendingCancelsForAgent_ReturnsOnlyOwnedAndPending(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobMineWithStamp, agentMine, _ := seedRunningAgentJob(t, pool)
	jobMineNoStamp, _, _ := seedRunningAgentJob(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET agent_id = $1 WHERE id = $2`,
		agentMine, jobMineNoStamp); err != nil {
		t.Fatalf("repoint second job to mine: %v", err)
	}
	jobOtherWithStamp, _, _ := seedRunningAgentJob(t, pool)

	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at = NOW() WHERE id IN ($1, $2)`,
		jobMineWithStamp, jobOtherWithStamp); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	got, err := s.ListPendingCancelsForAgent(ctx, agentMine)
	if err != nil {
		t.Fatalf("ListPendingCancelsForAgent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1; entries=%+v", len(got), got)
	}
	if got[0].JobRunID != jobMineWithStamp {
		t.Errorf("returned wrong job: got %s, want %s (mine+stamped)",
			got[0].JobRunID, jobMineWithStamp)
	}
	_ = jobMineNoStamp // explicitly unused in assertion; ensured to be excluded by count check.
}
