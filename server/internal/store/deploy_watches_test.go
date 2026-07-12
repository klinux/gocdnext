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

// seedWatchable sets up project → cluster → env → an in_progress deployment
// revision, and returns (projectID, revisionID) ready for a deploy_watch.
func seedWatchable(t *testing.T, s *store.Store, ctx context.Context, slug string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	projectID := seedProject(t, s, slug)
	if _, err := s.InsertCluster(ctx, newAuthCipher(t), store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure environment: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, Attempt: 0, Version: "v1", DeployedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("create revision: %v", err)
	}
	return projectID, revID
}

func newWatchInput(projectID, revID uuid.UUID) store.DeployWatchInput {
	return store.DeployWatchInput{
		DeploymentRevisionID: revID,
		ProjectID:            projectID,
		SyncMode:             "trigger",
		Cluster:              "prod-gke",
		Application:          "checkout",
		Namespace:            "argocd",
		ExpectedRevision:     "abc123",
		DeadlineAt:           time.Now().Add(10 * time.Minute),
	}
}

// The full watcher lifecycle plus the fencing guarantee: a watcher whose lease was
// reclaimed by another replica can neither renew nor terminalize the deploy.
func TestDeployWatch_ClaimRenewFinalize_Fencing(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "watch-life")

	w, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID))
	if err != nil {
		t.Fatalf("create watch: %v", err)
	}
	if w.ClaimID != uuid.Nil || w.SyncRequestedAt != nil || w.ExpectedRevision != "abc123" {
		t.Fatalf("fresh watch not unclaimed/pre-sync: %+v", w)
	}

	// Claim it (worker1) → gets a fencing token.
	claimed, err := s.ClaimDeployWatches(ctx, "worker1", 3600, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].DeploymentRevisionID != revID || claimed[0].ClaimID == uuid.Nil {
		t.Fatalf("claim = %+v, want the one watch with a token", claimed)
	}
	c1 := claimed[0].ClaimID

	// A fresh lease is NOT reclaimable by another replica.
	if again, err := s.ClaimDeployWatches(ctx, "worker2", 3600, 10); err != nil || len(again) != 0 {
		t.Fatalf("re-claim of a fresh lease = %v (err %v), want none", again, err)
	}

	// Correlation anchor + heartbeat under the held token.
	if ok, err := s.MarkDeployWatchSyncRequested(ctx, revID, c1); err != nil || !ok {
		t.Fatalf("mark sync-requested (held token): ok=%v err=%v", ok, err)
	}
	if ok, err := s.RenewDeployWatch(ctx, revID, c1); err != nil || !ok {
		t.Fatalf("renew (held token): ok=%v err=%v", ok, err)
	}

	// Simulate a takeover: a negative lease makes even a fresh claim reclaimable, so
	// worker2 steals it with a NEW token — no wall-clock sleep needed.
	stolen, err := s.ClaimDeployWatches(ctx, "worker2", -1, 10)
	if err != nil || len(stolen) != 1 {
		t.Fatalf("takeover claim = %v (err %v), want 1", stolen, err)
	}
	c2 := stolen[0].ClaimID
	if c2 == c1 {
		t.Fatalf("takeover reused the old token %v", c1)
	}

	// The old watcher is fenced out of BOTH renew and finalize.
	if ok, err := s.RenewDeployWatch(ctx, revID, c1); err != nil || ok {
		t.Fatalf("stale renew: ok=%v err=%v, want ok=false", ok, err)
	}
	if res, err := s.FinalizeDeployWatch(ctx, revID, c1, "success", ""); err != nil || res.Finalized {
		t.Fatalf("stale finalize: finalized=%v err=%v, want finalized=false", res.Finalized, err)
	}
	// The stale finalize must NOT have terminalized the deploy or removed the watch.
	if got, err := s.GetDeployWatch(ctx, revID); err != nil {
		t.Fatalf("watch gone after a fenced-out finalize: %v", err)
	} else if got.ClaimID != c2 {
		t.Fatalf("watch token = %v, want the live token %v", got.ClaimID, c2)
	}

	// The live watcher terminalizes atomically: revision → success, watch removed.
	if res, err := s.FinalizeDeployWatch(ctx, revID, c2, "success", ""); err != nil || !res.Finalized {
		t.Fatalf("live finalize: finalized=%v err=%v, want true", res.Finalized, err)
	}
	if _, err := s.GetDeployWatch(ctx, revID); err != store.ErrDeployWatchNotFound {
		t.Fatalf("GetDeployWatch after finalize = %v, want ErrDeployWatchNotFound", err)
	}
	rev, err := s.GetDeploymentRevision(ctx, revID)
	if err != nil {
		t.Fatalf("get revision: %v", err)
	}
	if rev.Status != "success" || rev.FinishedAt == nil {
		t.Fatalf("revision after finalize = %+v, want success + finished_at set", rev)
	}
}

// Degraded debounce anchor: opens on the first Degraded tick (earliest wins), clears
// on recovery — both fenced on the claim token.
func TestDeployWatch_DegradedDebounceToggle(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "watch-degraded")
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	claimed, err := s.ClaimDeployWatches(ctx, "w", 3600, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (err %v)", claimed, err)
	}
	c := claimed[0].ClaimID

	if ok, err := s.SetDeployWatchDegradedSince(ctx, revID, c); err != nil || !ok {
		t.Fatalf("set degraded: ok=%v err=%v", ok, err)
	}
	w, _ := s.GetDeployWatch(ctx, revID)
	if w.DegradedSince == nil {
		t.Fatal("degraded_since not set")
	}
	first := *w.DegradedSince

	// A second Set keeps the earliest anchor (COALESCE), not a fresh one.
	if _, err := s.SetDeployWatchDegradedSince(ctx, revID, c); err != nil {
		t.Fatalf("set degraded again: %v", err)
	}
	w, _ = s.GetDeployWatch(ctx, revID)
	if !w.DegradedSince.Equal(first) {
		t.Fatalf("degraded_since moved: %v -> %v (want stable)", first, *w.DegradedSince)
	}

	if ok, err := s.ClearDeployWatchDegraded(ctx, revID, c); err != nil || !ok {
		t.Fatalf("clear degraded: ok=%v err=%v", ok, err)
	}
	w, _ = s.GetDeployWatch(ctx, revID)
	if w.DegradedSince != nil {
		t.Fatalf("degraded_since not cleared: %v", *w.DegradedSince)
	}

	// A stale token can't touch the debounce state.
	if ok, _ := s.SetDeployWatchDegradedSince(ctx, revID, uuid.New()); ok {
		t.Fatal("stale token set degraded, want fenced out")
	}
}

// An in-flight watch counts toward the cluster delete-guard (also FK-RESTRICTed).
func TestDeployWatch_CountActiveForCluster(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "watch-count")

	if n, err := s.CountActiveWatchesForCluster(ctx, "prod-gke"); err != nil || n != 0 {
		t.Fatalf("count before = %d (err %v), want 0", n, err)
	}
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	if n, err := s.CountActiveWatchesForCluster(ctx, "prod-gke"); err != nil || n != 1 {
		t.Fatalf("count after = %d (err %v), want 1", n, err)
	}
}

// ListDeployWatchesForProject returns the project's in-flight native deploys joined to
// the environment name + display version.
func TestListDeployWatchesForProject(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "watch-list")
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}

	views, err := s.ListDeployWatchesForProject(ctx, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	v := views[0]
	if v.Environment != "production" || v.Version != "v1" || v.ExpectedRevision != "abc123" ||
		v.Application != "checkout" || v.Cluster != "prod-gke" || v.SyncMode != "trigger" {
		t.Fatalf("view = %+v, want the seeded in-flight deploy", v)
	}
	if v.SyncRequestedAt != nil {
		t.Errorf("SyncRequestedAt = %v, want nil (pre-Sync)", v.SyncRequestedAt)
	}

	// A different project sees none of it.
	other := seedProject(t, s, "watch-list-other")
	if got, err := s.ListDeployWatchesForProject(ctx, other); err != nil || len(got) != 0 {
		t.Fatalf("other project views = %v (err %v), want none", got, err)
	}
}

// A revision terminalized by the JOB/reaper path (not the watcher's own
// FinalizeDeployWatch) must still delete its watch atomically — otherwise the watch
// lingers in the live queue forever and falsely blocks deleting its cluster.
func TestFinalizeDeploymentRevision_DeletesWatch(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := newAuthCipher(t)
	s.SetAuthCipher(cipher)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure environment: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 0, Version: "v1", DeployedBy: "alice",
	})
	if err != nil {
		t.Fatalf("create revision: %v", err)
	}
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	if n, _ := s.CountActiveWatchesForCluster(ctx, "prod-gke"); n != 1 {
		t.Fatalf("precondition watches = %d, want 1", n)
	}

	// Terminalize via the JOB path.
	if n, err := s.FinalizeDeploymentRevision(ctx, jobID, 0, store.DeployStatusSuccess); err != nil || n != 1 {
		t.Fatalf("finalize by job = %d (err %v), want 1", n, err)
	}
	// The watch is atomically gone — not orphaned in the live queue or the count.
	if _, err := s.GetDeployWatch(ctx, revID); err != store.ErrDeployWatchNotFound {
		t.Fatalf("watch after job-finalize = %v, want ErrDeployWatchNotFound", err)
	}
	if n, _ := s.CountActiveWatchesForCluster(ctx, "prod-gke"); n != 0 {
		t.Fatalf("watches after job-finalize = %d, want 0 (orphan cleaned)", n)
	}
}

// seedServerManagedDeploy sets up a running deploy job with NO agent (Model A) +
// its revision + a claimed watch, and returns (runID, jobID, revID, claimID).
func seedServerManagedDeploy(t *testing.T, s *store.Store, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE job_runs SET agent_id=NULL, started_at=NOW() WHERE id=$1`, jobID); err != nil {
		t.Fatalf("orphan job: %v", err)
	}
	projectID := projectIDForRun(t, pool, runID)
	if _, err := s.InsertCluster(ctx, newAuthCipher(t), store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("cluster: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 0, Version: "v1", DeployedBy: "svc",
	})
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	if _, err := s.CreateDeployWatch(ctx, store.DeployWatchInput{
		DeploymentRevisionID: revID, ProjectID: projectID, SyncMode: "trigger",
		Cluster: "prod-gke", Application: "checkout", Namespace: "argocd",
		ExpectedRevision: "v1", DeadlineAt: time.Now().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	claimed, err := s.ClaimDeployWatches(ctx, "w", 3600, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (%v)", claimed, err)
	}
	return runID, jobID, revID, claimed[0].ClaimID
}

// FinalizeDeployWatch completes the server-managed job_run atomically with the watch
// + revision, so the job status equals the deploy outcome (ADR-0001, Model A).
func TestFinalizeDeployWatch_CompletesServerManagedJob(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()
	runID, jobID, revID, claimID := seedServerManagedDeploy(t, s, pool)

	res, err := s.FinalizeDeployWatch(ctx, revID, claimID, store.DeployStatusSuccess, "")
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !res.Finalized || res.RunID != runID {
		t.Fatalf("res = %+v, want finalized with run %v", res, runID)
	}
	var jobStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&jobStatus)
	if jobStatus != "success" {
		t.Fatalf("job status = %q, want success (watcher completed it)", jobStatus)
	}
	if rev, _ := s.GetDeploymentRevision(ctx, revID); rev.Status != store.DeployStatusSuccess {
		t.Fatalf("revision = %q, want success", rev.Status)
	}
	if _, err := s.GetDeployWatch(ctx, revID); err != store.ErrDeployWatchNotFound {
		t.Fatalf("watch not gone: %v", err)
	}
}

func TestFinalizeDeployWatch_FailedJobCarriesReason(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()
	_, jobID, revID, claimID := seedServerManagedDeploy(t, s, pool)

	if _, err := s.FinalizeDeployWatch(ctx, revID, claimID, store.DeployStatusFailed, "degraded beyond debounce window"); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var status string
	var errMsg *string
	var exit *int32
	_ = pool.QueryRow(ctx, `SELECT status, error, exit_code FROM job_runs WHERE id=$1`, jobID).Scan(&status, &errMsg, &exit)
	if status != "failed" || errMsg == nil || *errMsg != "degraded beyond debounce window" {
		t.Fatalf("job = %q err=%v, want failed + the reason", status, errMsg)
	}
	if exit == nil || *exit != 1 {
		t.Fatalf("exit_code = %v, want 1 for a failed deploy", exit)
	}
}

// A deploy job that still has an AGENT (not server-managed) must NOT be completed by
// the watcher — only the revision + watch terminalize; the job is left to its agent.
func TestFinalizeDeployWatch_LeavesAgentRunJobAlone(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := newAuthCipher(t)
	s.SetAuthCipher(cipher)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool) // compile is running WITH an agent
	projectID := projectIDForRun(t, pool, runID)
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("cluster: %v", err)
	}
	envID, _ := s.EnsureEnvironment(ctx, projectID, "production")
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 0, Version: "v1", DeployedBy: "svc",
	})
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	if _, err := s.CreateDeployWatch(ctx, store.DeployWatchInput{
		DeploymentRevisionID: revID, ProjectID: projectID, SyncMode: "trigger",
		Cluster: "prod-gke", Application: "checkout", Namespace: "argocd",
		ExpectedRevision: "v1", DeadlineAt: time.Now().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	claimed, _ := s.ClaimDeployWatches(ctx, "w", 3600, 10)

	res, err := s.FinalizeDeployWatch(ctx, revID, claimed[0].ClaimID, store.DeployStatusSuccess, "")
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !res.Finalized || res.RunID != uuid.Nil {
		t.Fatalf("res = %+v, want finalized with NO run (agent-run job untouched)", res)
	}
	var jobStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&jobStatus)
	if jobStatus != "running" {
		t.Fatalf("agent-run job status = %q, want running (untouched)", jobStatus)
	}
}

// CreateDeployWatch refuses to watch an already-terminal revision (a late/duplicate
// create): there is nothing to observe.
func TestCreateDeployWatch_RejectsTerminalRevision(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "watch-terminal")

	// Drive the revision terminal through a full watch cycle.
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	claimed, err := s.ClaimDeployWatches(ctx, "w", 3600, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (err %v)", claimed, err)
	}
	if res, err := s.FinalizeDeployWatch(ctx, revID, claimed[0].ClaimID, "success", ""); err != nil || !res.Finalized {
		t.Fatalf("finalize: fin=%v err=%v", res.Finalized, err)
	}

	// The revision is terminal now → a new watch is refused.
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != store.ErrRevisionNotInProgress {
		t.Fatalf("create watch on terminal revision = %v, want ErrRevisionNotInProgress", err)
	}
}

// FinalizeDeployWatch validates status up front (mirrors FinalizeDeploymentRevision),
// leaving the watch untouched rather than aborting on the DB status CHECK.
func TestFinalizeDeployWatch_InvalidStatus(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "watch-badstatus")
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	claimed, err := s.ClaimDeployWatches(ctx, "w", 3600, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (err %v)", claimed, err)
	}
	if _, err := s.FinalizeDeployWatch(ctx, revID, claimed[0].ClaimID, "canceled", ""); err == nil {
		t.Fatal("finalize with invalid status = nil error, want a validation error")
	}
	// The watch must survive (validation happened before any DB effect).
	if _, err := s.GetDeployWatch(ctx, revID); err != nil {
		t.Fatalf("watch gone after an invalid-status finalize: %v", err)
	}
}
