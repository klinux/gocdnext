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

// seedDeclaredTakeover builds a run + cluster + registered target so a DECLARED takeover
// has something to lock. Returns the ids the assertions need.
func seedDeclaredTakeover(t *testing.T, pool *pgxpool.Pool, s *store.Store) (runID, jobUnitID, projectID, envID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	runID, _, _, _, jobUnitID = seedRunningJob(t, pool)
	projectID = projectIDForRun(t, pool, runID)
	if _, err := s.InsertCluster(ctx, newAuthCipher(t), store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("cluster: %v", err)
	}
	var err error
	if envID, err = s.EnsureEnvironment(ctx, projectID, "production"); err != nil {
		t.Fatalf("env: %v", err)
	}
	if err := s.UpsertDeployTarget(ctx, store.DeployTargetInput{
		EnvironmentID: envID, Provider: "argocd", Cluster: "prod-gke",
		Application: "checkout", Namespace: "argocd", SyncMode: "trigger",
	}); err != nil {
		t.Fatalf("target: %v", err)
	}
	return runID, jobUnitID, projectID, envID
}

func declaredInput(runID, jobUnitID, projectID, envID uuid.UUID) store.StartNativeDeployInput {
	return store.StartNativeDeployInput{
		JobRunID: jobUnitID, EnvironmentID: envID, RunID: runID, Version: "v1", DeployedBy: "svc",
		ProjectID: projectID, SyncMode: "trigger", Cluster: "prod-gke", Application: "checkout",
		Namespace: "argocd", ExpectedRevision: "v1", DeadlineAt: time.Now().Add(10 * time.Minute),
	}
}

func wantExpectation() store.DeclaredTargetExpectation {
	return store.DeclaredTargetExpectation{
		Environment: "production", Cluster: "prod-gke",
		Application: "checkout", Namespace: "argocd", SyncMode: "trigger",
	}
}

// assertNoTakeoverEffects is the assertion that matters: a refused declared takeover must
// leave NOTHING behind. A partial effect (job flipped but no watch, or a revision with no
// watch) is the failure mode the transaction exists to prevent.
func assertNoTakeoverEffects(t *testing.T, pool *pgxpool.Pool, jobUnitID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobUnitID).Scan(&status); err != nil {
		t.Fatalf("job row: %v", err)
	}
	if status == "running" {
		t.Error("job was flipped to running despite a refused takeover")
	}
	var revisions, watches int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM deployment_revisions WHERE job_run_id=$1`, jobUnitID).Scan(&revisions)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM deploy_watches dw
		JOIN deployment_revisions dr ON dr.id = dw.deployment_revision_id
		WHERE dr.job_run_id=$1`, jobUnitID).Scan(&watches)
	if revisions != 0 || watches != 0 {
		t.Errorf("revisions=%d watches=%d, want 0/0 — a refused takeover left state behind", revisions, watches)
	}
}

func TestStartNativeDeployDeclared_HappyPath(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	runID, jobUnitID, projectID, envID := seedDeclaredTakeover(t, pool, s)

	res, outcome, err := s.StartNativeDeployDeclared(context.Background(),
		declaredInput(runID, jobUnitID, projectID, envID), wantExpectation())
	if err != nil {
		t.Fatalf("declared takeover: %v", err)
	}
	if outcome != store.DeclaredTakeoverStarted || !res.Started {
		t.Fatalf("outcome = %v res = %+v, want started", outcome, res)
	}
}

// THE race the row lock exists for: a gate added between the reconcile and the takeover
// must not produce an UNGATED deploy. StartNativeDeploy never re-reads deploy_targets,
// so checking "just before" would not catch this.
func TestStartNativeDeployDeclared_GateInWindowBlocksEverything(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()
	runID, jobUnitID, projectID, envID := seedDeclaredTakeover(t, pool, s)

	// An admin gates the target after the reconcile decided it was ungated.
	if _, err := pool.Exec(ctx,
		`UPDATE deploy_targets SET governing_gate = '{"required":1}'::jsonb WHERE environment_id=$1`,
		envID); err != nil {
		t.Fatalf("gate: %v", err)
	}

	_, outcome, err := s.StartNativeDeployDeclared(ctx,
		declaredInput(runID, jobUnitID, projectID, envID), wantExpectation())
	if err != nil {
		t.Fatalf("declared takeover: %v", err)
	}
	if outcome != store.DeclaredTakeoverGated {
		t.Fatalf("outcome = %v, want Gated (terminal)", outcome)
	}
	assertNoTakeoverEffects(t, pool, jobUnitID)
}

// A base edit in the same window is a benign race with a concurrent change — retry, do
// not fail. Table-driven over EVERY guarded field: a guard like this ends up "almost
// right" when only the obvious one is exercised.
func TestStartNativeDeployDeclared_BaseDriftInWindowRetries(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"application", `UPDATE deploy_targets SET application='other' WHERE environment_id=$1`},
		{"namespace", `UPDATE deploy_targets SET namespace='argocd-2' WHERE environment_id=$1`},
		{"sync_mode", `UPDATE deploy_targets SET sync_mode='observe' WHERE environment_id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := dbtest.SetupPool(t)
			s := store.New(pool)
			s.SetAuthCipher(newAuthCipher(t))
			ctx := context.Background()
			runID, jobUnitID, projectID, envID := seedDeclaredTakeover(t, pool, s)

			if _, err := pool.Exec(ctx, tc.sql, envID); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			_, outcome, err := s.StartNativeDeployDeclared(ctx,
				declaredInput(runID, jobUnitID, projectID, envID), wantExpectation())
			if err != nil {
				t.Fatalf("declared takeover: %v", err)
			}
			if outcome != store.DeclaredTakeoverDrifted {
				t.Fatalf("outcome = %v, want Drifted (retry)", outcome)
			}
			assertNoTakeoverEffects(t, pool, jobUnitID)
		})
	}
}

// The watch is built from the LOCKED row, not the caller's pre-lock snapshot: rollout
// routing changed since the reconcile must be ADOPTED (YAML does not own it), never
// written back from a stale read.
func TestStartNativeDeployDeclared_AdoptsRolloutRoutingFromTheLockedRow(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()
	runID, jobUnitID, projectID, envID := seedDeclaredTakeover(t, pool, s)

	if _, err := pool.Exec(ctx,
		`UPDATE deploy_targets SET rollout_aware=true, rollout_name='shop-canary' WHERE environment_id=$1`,
		envID); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// The caller's input still carries the PRE-pin values (rollout_aware=false).
	_, outcome, err := s.StartNativeDeployDeclared(ctx,
		declaredInput(runID, jobUnitID, projectID, envID), wantExpectation())
	if err != nil {
		t.Fatalf("declared takeover: %v", err)
	}
	if outcome != store.DeclaredTakeoverStarted {
		t.Fatalf("outcome = %v, want Started (rollout routing is not compared)", outcome)
	}
	var aware bool
	var name *string
	if err := pool.QueryRow(ctx, `SELECT dw.rollout_aware, dw.rollout_name
		FROM deploy_watches dw
		JOIN deployment_revisions dr ON dr.id = dw.deployment_revision_id
		WHERE dr.job_run_id=$1`, jobUnitID).Scan(&aware, &name); err != nil {
		t.Fatalf("watch row: %v", err)
	}
	if !aware || name == nil || *name != "shop-canary" {
		t.Fatalf("watch rollout_aware=%v name=%v, want the LOCKED row's values", aware, name)
	}
}
