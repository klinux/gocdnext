package scheduler_test

import (
	"context"
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
