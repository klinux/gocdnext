package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// StartNativeDeploy flips a queued job to server-managed running (no agent) and
// records the revision + watch — all in one commit.
func TestStartNativeDeploy_Atomic(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	runID, _, _, _, jobUnitID := seedRunningJob(t, pool) // unit stays queued
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

	res, err := s.StartNativeDeploy(ctx, store.StartNativeDeployInput{
		JobRunID: jobUnitID, EnvironmentID: envID, RunID: runID, Version: "v1", DeployedBy: "svc",
		ProjectID: projectID, SyncMode: "trigger", Cluster: "prod-gke", Application: "checkout",
		Namespace: "argocd", ExpectedRevision: "v1", DeadlineAt: time.Now().Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("StartNativeDeploy: %v", err)
	}
	if !res.Started || res.RevisionID == uuid.Nil {
		t.Fatalf("res = %+v, want started with a revision id", res)
	}

	// Job is running with NO agent.
	var status string
	var agent *string
	_ = pool.QueryRow(ctx, `SELECT status, agent_id::text FROM job_runs WHERE id=$1`, jobUnitID).Scan(&status, &agent)
	if status != "running" || agent != nil {
		t.Fatalf("job = %q agent=%v, want running + no agent", status, agent)
	}
	// Revision is in_progress and linked to the job.
	rev, err := s.GetDeploymentRevision(ctx, res.RevisionID)
	if err != nil {
		t.Fatalf("get revision: %v", err)
	}
	if rev.Status != store.DeployStatusInProgress || rev.JobRunID == nil || *rev.JobRunID != jobUnitID {
		t.Fatalf("revision = %+v, want in_progress linked to the job", rev)
	}
	// Watch exists, pre-Sync.
	w, err := s.GetDeployWatch(ctx, res.RevisionID)
	if err != nil {
		t.Fatalf("get watch: %v", err)
	}
	if w.SyncRequestedAt != nil || w.Application != "checkout" || w.ExpectedRevision != "v1" {
		t.Fatalf("watch = %+v, want pre-Sync with the target", w)
	}
	// The stage AND run are promoted to running in the same tx (invariant:
	// server-managed job running ⇒ stage/run running; serial gating keys on it).
	var stageStatus, runStatus string
	_ = pool.QueryRow(ctx, `
		SELECT sr.status, r.status
		FROM job_runs j
		JOIN stage_runs sr ON sr.id = j.stage_run_id
		JOIN runs r ON r.id = j.run_id
		WHERE j.id = $1`, jobUnitID).Scan(&stageStatus, &runStatus)
	if stageStatus != "running" || runStatus != "running" {
		t.Fatalf("stage=%q run=%q, want both running after native takeover", stageStatus, runStatus)
	}
}

// A job that isn't dispatchable (already running) → Started=false, nothing created.
func TestStartNativeDeploy_LostRace(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	runID, _, _, jobCompileID, _ := seedRunningJob(t, pool) // compile already running
	projectID := projectIDForRun(t, pool, runID)
	if _, err := s.InsertCluster(ctx, newAuthCipher(t), store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("cluster: %v", err)
	}
	envID, _ := s.EnsureEnvironment(ctx, projectID, "production")

	res, err := s.StartNativeDeploy(ctx, store.StartNativeDeployInput{
		JobRunID: jobCompileID, EnvironmentID: envID, RunID: runID, Version: "v1", DeployedBy: "svc",
		ProjectID: projectID, SyncMode: "trigger", Cluster: "prod-gke", Application: "checkout",
		Namespace: "argocd", ExpectedRevision: "v1", DeadlineAt: time.Now().Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("StartNativeDeploy: %v", err)
	}
	if res.Started {
		t.Fatal("Started=true for an already-running job, want false (lost race)")
	}
	// No orphan watch was created.
	if _, err := s.GetDeployWatch(ctx, res.RevisionID); err != store.ErrDeployWatchNotFound {
		t.Fatalf("a watch was created despite the lost race: %v", err)
	}
}

// The unfenced dispatch stamp is monotonic: it sets the anchor once and never reopens it.
func TestStampDeployWatchSyncRequested_Monotonic(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "stamp-mono")
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}

	if ok, err := s.StampDeployWatchSyncRequested(ctx, revID); err != nil || !ok {
		t.Fatalf("first stamp: ok=%v err=%v, want true", ok, err)
	}
	w, _ := s.GetDeployWatch(ctx, revID)
	if w.SyncRequestedAt == nil {
		t.Fatal("sync_requested_at not set")
	}
	first := *w.SyncRequestedAt

	// A second stamp is a no-op (monotonic) — anchor unchanged.
	if ok, err := s.StampDeployWatchSyncRequested(ctx, revID); err != nil || ok {
		t.Fatalf("second stamp: ok=%v err=%v, want false (already set)", ok, err)
	}
	w, _ = s.GetDeployWatch(ctx, revID)
	if !w.SyncRequestedAt.Equal(first) {
		t.Fatalf("anchor moved: %v -> %v (want stable)", first, *w.SyncRequestedAt)
	}
}
