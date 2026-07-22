package scheduler_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/deploysvc"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

type fakeSyncer struct{ calls int }

func (f *fakeSyncer) Sync(_ context.Context, _ deploy.DeploymentTarget, _ string) error {
	f.calls++
	return nil
}

func projectIDForSlug(t *testing.T, pool *pgxpool.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM projects WHERE slug=$1`, slug).Scan(&id); err != nil {
		t.Fatalf("project id for %q: %v", slug, err)
	}
	return id
}

func registerDeployTarget(t *testing.T, s *store.Store, projectID uuid.UUID, env, syncMode string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.InsertCluster(ctx, nil, store.ClusterInput{
		Name: "prod-cluster", AuthType: store.ClusterAuthInCluster,
	}); err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, env)
	if err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	if err := s.UpsertDeployTarget(ctx, store.DeployTargetInput{
		EnvironmentID: envID, Provider: "argocd", Cluster: "prod-cluster",
		Application: "checkout", Namespace: "argocd", SyncMode: syncMode, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("upsert deploy target: %v", err)
	}
}

// With a deploy_target registered, a deploy job is taken over natively: it becomes
// server-managed (running, NO agent, owning watch) and is NOT dispatched to an agent;
// the sync fires.
func TestDispatchRun_NativeTakeover(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sync := &fakeSyncer{}
	sched := scheduler.New(s, sessions, quietLogger(), testDSN).
		WithNativeDeployer(deploysvc.NewNativeDeployer(sync, s, quietLogger()))
	ctx := context.Background()

	_, run, _ := seedDeployRuns(t, pool, "native-take", domain.SupersedeOff)
	jobID := soleJobID(t, run)
	registerDeployTarget(t, s, projectIDForSlug(t, pool, "native-take"), "prod", "trigger")

	agentID := seedAgentRow(t, pool, "native-take-agent")
	sess := sessions.CreateSession(agentID, nil, 1, 0)

	sched.DispatchRun(ctx, run.RunID)

	// No agent frame — the job never went to the agent.
	assertNoAssignment(t, sess)

	// Server-managed: running with no agent.
	var status string
	var agent *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT status, agent_id FROM job_runs WHERE id=$1`, jobID).Scan(&status, &agent); err != nil {
		t.Fatalf("job row: %v", err)
	}
	if status != "running" || agent != nil {
		t.Fatalf("job = %q agent=%v, want running + no agent (server-managed)", status, agent)
	}
	// A revision + live watch own it, and the sync fired.
	var watches int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM deploy_watches dw
		JOIN deployment_revisions dr ON dr.id = dw.deployment_revision_id
		WHERE dr.job_run_id=$1`, jobID).Scan(&watches); err != nil {
		t.Fatalf("watch count: %v", err)
	}
	if watches != 1 {
		t.Fatalf("deploy_watches for the job = %d, want 1", watches)
	}
	if sync.calls != 1 {
		t.Fatalf("sync called %d times, want 1 (trigger mode)", sync.calls)
	}
}

// seedDeployRunVersioned applies a one-job deploy pipeline with an explicit
// deploy.version and creates a single run on commit aaa…a.
func seedDeployRunVersioned(t *testing.T, pool *pgxpool.Pool, slug, version string) (runID, jobID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	fp := domain.GitFingerprint("https://github.com/org/"+slug, "main")
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "deploy", Supersede: domain.SupersedeOff, Stages: []string{"deploy"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/" + slug, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{
				Name: "ship", Stage: "deploy", Image: "alpine:3.19",
				Tasks:  []domain.Task{{Script: "echo deploy"}},
				Deploy: &domain.DeploySpec{Environment: "prod", Version: version},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID,
		Revision: "aaa0123456789aaa0123456789aaa0123456789a", Branch: "main",
		Provider: "github", Delivery: slug, TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return run.RunID, soleJobID(t, run)
}

// An explicit full-SHA deploy.version is honored as the correlation anchor (a
// deliberately pinned commit, even a different one than the run's).
func TestDispatchRun_NativeExplicitFullSHA(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sched := scheduler.New(s, grpcsrv.NewSessionStore(), quietLogger(), testDSN).
		WithNativeDeployer(deploysvc.NewNativeDeployer(&fakeSyncer{}, s, quietLogger()))
	ctx := context.Background()

	const pinned = "bbb0123456789bbb0123456789bbb0123456789b"
	runID, jobID := seedDeployRunVersioned(t, pool, "native-fullsha", pinned)
	registerDeployTarget(t, s, projectIDForSlug(t, pool, "native-fullsha"), "prod", "trigger")

	sched.DispatchRun(ctx, runID)

	var expectedRev string
	if err := pool.QueryRow(ctx, `
		SELECT dw.expected_revision FROM deploy_watches dw
		JOIN deployment_revisions dr ON dr.id = dw.deployment_revision_id
		WHERE dr.job_run_id=$1`, jobID).Scan(&expectedRev); err != nil {
		t.Fatalf("watch expected_revision: %v", err)
	}
	if expectedRev != pinned {
		t.Fatalf("expected_revision = %q, want the pinned full SHA %q", expectedRev, pinned)
	}
}

// An explicit non-correlatable version (semver) fails the native deploy TERMINALLY —
// no watch, no sync, no unpinned false-success.
func TestDispatchRun_NativeNonCorrelatableVersionFails(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sync := &fakeSyncer{}
	sched := scheduler.New(s, grpcsrv.NewSessionStore(), quietLogger(), testDSN).
		WithNativeDeployer(deploysvc.NewNativeDeployer(sync, s, quietLogger()))
	ctx := context.Background()

	runID, jobID := seedDeployRunVersioned(t, pool, "native-semver", "1.2.3")
	registerDeployTarget(t, s, projectIDForSlug(t, pool, "native-semver"), "prod", "trigger")

	sched.DispatchRun(ctx, runID)

	var status string
	var errMsg *string
	if err := pool.QueryRow(ctx, `SELECT status, error FROM job_runs WHERE id=$1`, jobID).Scan(&status, &errMsg); err != nil {
		t.Fatalf("job row: %v", err)
	}
	if status != "failed" {
		t.Fatalf("job status = %q, want failed (non-correlatable version)", status)
	}
	if errMsg == nil || !strings.Contains(*errMsg, "correlatable") {
		t.Fatalf("job error = %v, want the not-correlatable message", errMsg)
	}
	if sync.calls != 0 {
		t.Fatal("a non-correlatable version must not sync")
	}
	var watches int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM deploy_watches dw
		JOIN deployment_revisions dr ON dr.id = dw.deployment_revision_id WHERE dr.job_run_id=$1`, jobID).Scan(&watches)
	if watches != 0 {
		t.Fatalf("deploy_watches = %d, want 0 (no watch on a failed native deploy)", watches)
	}
}

// The core HIGH-2 fix: a native deploy needs NO agent. With a target registered and
// NO idle agent in the pool, it must still be taken over (server-managed), not sit
// queued waiting for an agent it never uses.
func TestDispatchRun_NativeTakeover_NoIdleAgent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore() // deliberately EMPTY — no agent
	sync := &fakeSyncer{}
	sched := scheduler.New(s, sessions, quietLogger(), testDSN).
		WithNativeDeployer(deploysvc.NewNativeDeployer(sync, s, quietLogger()))
	ctx := context.Background()

	_, run, _ := seedDeployRuns(t, pool, "native-noagent", domain.SupersedeOff)
	jobID := soleJobID(t, run)
	registerDeployTarget(t, s, projectIDForSlug(t, pool, "native-noagent"), "prod", "trigger")

	sched.DispatchRun(ctx, run.RunID)

	var status string
	var agent *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT status, agent_id FROM job_runs WHERE id=$1`, jobID).Scan(&status, &agent); err != nil {
		t.Fatalf("job row: %v", err)
	}
	if status != "running" || agent != nil {
		t.Fatalf("job = %q agent=%v, want running + no agent (taken over WITHOUT an idle agent)", status, agent)
	}
	// The run itself is promoted to running — serial gating keys on runs.status, so a
	// native deploy in flight must not read as queued (another run could start).
	var runStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, run.RunID).Scan(&runStatus)
	if runStatus != "running" {
		t.Fatalf("run status = %q, want running after native takeover", runStatus)
	}
	if sync.calls != 1 {
		t.Fatalf("sync called %d times, want 1", sync.calls)
	}
	// The correlation anchor is the FULL commit SHA (not the short-sha display version).
	var expectedRev string
	if err := pool.QueryRow(ctx, `
		SELECT dw.expected_revision FROM deploy_watches dw
		JOIN deployment_revisions dr ON dr.id = dw.deployment_revision_id
		WHERE dr.job_run_id=$1`, jobID).Scan(&expectedRev); err != nil {
		t.Fatalf("watch expected_revision: %v", err)
	}
	if expectedRev != "aaa0123456789aaa0123456789aaa0123456789a" {
		t.Fatalf("expected_revision = %q, want the full commit SHA (HIGH 3)", expectedRev)
	}
}

// REGRESSION (v0.72.0): the native-only git-SHA rule must never judge a TRACKING-LAYER
// deploy. The takeover used to resolve the marker — which enforces correlatability —
// BEFORE settling whether a target exists, so a legacy "1.<counter>.<short-sha>" image
// tag (explicitly valid for the tracking layer) failed the job terminally instead of
// falling through to the plugin path. Same shape as
// TestDispatchRun_NativeNonCorrelatableVersionFails, minus the registered target: the
// version is identical, only the target's absence decides.
func TestDispatchRun_TrackingLayerAcceptsNonSHAVersion(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sync := &fakeSyncer{}
	sched := scheduler.New(s, sessions, quietLogger(), testDSN).
		WithNativeDeployer(deploysvc.NewNativeDeployer(sync, s, quietLogger()))
	ctx := context.Background()

	// A GoCD-style image tag as deploy.version, and deliberately NO registerDeployTarget.
	runID, jobID := seedDeployRunVersioned(t, pool, "tracking-legacy", "1.27.1f2403ea")

	agentID := seedAgentRow(t, pool, "tracking-legacy-agent")
	_ = sessions.CreateSession(agentID, nil, 1, 0)

	sched.DispatchRun(ctx, runID)

	var status string
	var errMsg *string
	var agent *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT status, error, agent_id FROM job_runs WHERE id=$1`, jobID).
		Scan(&status, &errMsg, &agent); err != nil {
		t.Fatalf("job row: %v", err)
	}
	if status == "failed" {
		t.Fatalf("job failed (%v) — a tracking-layer deploy must not be judged by the native git-SHA rule", errMsg)
	}
	if status != "running" || agent == nil || *agent != agentID {
		t.Fatalf("job = %q agent=%v, want running on the agent (plugin path)", status, agent)
	}
	if sync.calls != 0 {
		t.Fatalf("sync called %d times, want 0 (tracking layer must not sync)", sync.calls)
	}
}

// With the native deployer wired but NO target registered, the deploy job falls back
// to the plugin path — a normal agent dispatch.
func TestDispatchRun_NativeFallbackWhenNoTarget(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sync := &fakeSyncer{}
	sched := scheduler.New(s, sessions, quietLogger(), testDSN).
		WithNativeDeployer(deploysvc.NewNativeDeployer(sync, s, quietLogger()))
	ctx := context.Background()

	_, run, _ := seedDeployRuns(t, pool, "native-fallback", domain.SupersedeOff)
	jobID := soleJobID(t, run)
	// No registerDeployTarget → ErrDeployTargetNotFound → plugin fallback.

	agentID := seedAgentRow(t, pool, "native-fallback-agent")
	_ = sessions.CreateSession(agentID, nil, 1, 0)

	sched.DispatchRun(ctx, run.RunID)

	// Fallback = normal dispatch: the job was assigned to the agent, and no watch exists.
	var status string
	var agent *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT status, agent_id FROM job_runs WHERE id=$1`, jobID).Scan(&status, &agent); err != nil {
		t.Fatalf("job row: %v", err)
	}
	if status != "running" || agent == nil || *agent != agentID {
		t.Fatalf("job = %q agent=%v, want running on the agent (plugin fallback)", status, agent)
	}
	if sync.calls != 0 {
		t.Fatalf("sync called %d times, want 0 (fallback must not sync)", sync.calls)
	}
	var watches int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM deploy_watches dw
		JOIN deployment_revisions dr ON dr.id = dw.deployment_revision_id
		WHERE dr.job_run_id=$1`, jobID).Scan(&watches)
	if watches != 0 {
		t.Fatalf("deploy_watches = %d, want 0 for the plugin path", watches)
	}
}
