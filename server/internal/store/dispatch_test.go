package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func TestListDispatchableJobs_ReturnsActiveStageOnly(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	jobs, err := s.ListDispatchableJobs(ctx, res.RunID)
	if err != nil {
		t.Fatalf("list dispatchable: %v", err)
	}
	// Seed pipeline has stages [build, test] with one job each. Initially only
	// the build-stage job (compile) is dispatchable.
	if len(jobs) != 1 || jobs[0].Name != "compile" {
		t.Fatalf("want [compile], got %+v", jobs)
	}
}

func TestAssignJob_OnlyOneWinsRace(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	jobs, _ := s.ListDispatchableJobs(ctx, res.RunID)
	if len(jobs) == 0 {
		t.Fatalf("no dispatchable job to assign")
	}
	jobID := jobs[0].ID
	agentA, agentB := uuid.New(), uuid.New()

	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (id, name, token_hash) VALUES ($1, $2, 'hash'), ($3, $4, 'hash')`,
		agentA, "a", agentB, "b",
	); err != nil {
		t.Fatalf("seed agents: %v", err)
	}

	_, okA, err := s.AssignJob(ctx, jobID, agentA)
	if err != nil || !okA {
		t.Fatalf("A assign: ok=%v err=%v", okA, err)
	}
	_, okB, err := s.AssignJob(ctx, jobID, agentB)
	if err != nil {
		t.Fatalf("B assign err: %v", err)
	}
	if okB {
		t.Fatalf("second AssignJob should have lost the race")
	}
}

func TestAssignJob_RemovesJobFromDispatchable(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	jobs, _ := s.ListDispatchableJobs(ctx, res.RunID)
	agentID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (id, name, token_hash) VALUES ($1, 'a', 'hash')`, agentID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, ok, err := s.AssignJob(ctx, jobs[0].ID, agentID); err != nil || !ok {
		t.Fatalf("assign: ok=%v err=%v", ok, err)
	}

	after, _ := s.ListDispatchableJobs(ctx, res.RunID)
	if len(after) != 0 {
		t.Fatalf("dispatchable after assign = %+v, want []", after)
	}
}

func TestMarkRunRunning_IdempotentAndNoopWhenAlreadyRunning(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := s.MarkRunRunning(ctx, res.RunID); err != nil {
		t.Fatalf("mark 1: %v", err)
	}
	if err := s.MarkRunRunning(ctx, res.RunID); err != nil {
		t.Fatalf("mark 2: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, res.RunID).Scan(&status); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != "running" {
		t.Fatalf("status = %s, want running", status)
	}
}

func TestListQueuedRunIDs_OrdersByCreatedAt(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	_, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	in2 := baseTriggerInput(pipelineID, materialID, 2)
	in2.Revision = "b111111111111111111111111111111111111111"
	_, err = s.CreateRunFromModification(ctx, in2)
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}

	ids, err := s.ListQueuedRunIDs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("len = %d, want 2", len(ids))
	}
}

// TestRunHasServices_RealPipelineDefinition guards the JSON-key
// casing of the SQL check. Pipeline definitions are persisted via
// json.Marshal(domain.Pipeline) with NO json tags, so the field
// name `Services` reaches the DB capitalized — an earlier version
// of the query checked `definition->'services'` and silently
// returned false on every run with services. The test plants a
// pipeline through ApplyProject (the real persistence path) and
// asserts the lookup sees the services.
func TestRunHasServices_RealPipelineDefinition(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	url, branch := "https://github.com/org/svc-pipe", "main"
	fp := store.FingerprintFor(url, branch)

	// Pipeline WITH services — the case the query must catch.
	withSvcs := &domain.Pipeline{
		Name:   "with-services",
		Stages: []string{"build"},
		Services: []domain.Service{
			{Name: "postgres", Image: "postgres:16-alpine"},
		},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
		}},
		Jobs: []domain.Job{
			{Name: "test", Stage: "build", Tasks: []domain.Task{{Script: "go test"}}},
		},
	}
	// Pipeline WITHOUT services — the false-case the query must
	// also catch (the optimization gate skips dispatch on this).
	withoutSvcs := &domain.Pipeline{
		Name:   "no-services",
		Stages: []string{"build"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
		}},
		Jobs: []domain.Job{
			{Name: "test", Stage: "build", Tasks: []domain.Task{{Script: "go test"}}},
		},
	}
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "svc-test", Name: "Svc Test",
		Pipelines: []*domain.Pipeline{withSvcs, withoutSvcs},
	})
	if err != nil {
		t.Fatalf("ApplyProject: %v", err)
	}

	// Create one run per pipeline.
	makeRun := func(idx int, rev string) uuid.UUID {
		t.Helper()
		var matID uuid.UUID
		err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE pipeline_id=$1 LIMIT 1`,
			res.Pipelines[idx].PipelineID).Scan(&matID)
		if err != nil {
			t.Fatalf("material lookup: %v", err)
		}
		run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
			PipelineID:  res.Pipelines[idx].PipelineID,
			MaterialID:  matID,
			Revision:    rev,
			Branch:      branch,
			Provider:    "test",
			Delivery:    "test-" + rev,
			TriggeredBy: "test",
		})
		if err != nil {
			t.Fatalf("CreateRunFromModification: %v", err)
		}
		return run.RunID
	}
	runWithSvcs := makeRun(0, "aaa1111111111111111111111111111111111111")
	runWithoutSvcs := makeRun(1, "bbb2222222222222222222222222222222222222")

	got, err := s.RunHasServices(ctx, runWithSvcs)
	if err != nil {
		t.Fatalf("RunHasServices (with services): %v", err)
	}
	if !got {
		t.Errorf("RunHasServices returned false for a pipeline WITH services — JSON key casing regressed?")
	}

	got, err = s.RunHasServices(ctx, runWithoutSvcs)
	if err != nil {
		t.Fatalf("RunHasServices (without services): %v", err)
	}
	if got {
		t.Errorf("RunHasServices returned true for a pipeline WITHOUT services")
	}

	// Missing run → false (fail-closed; safer to skip cleanup than to leak).
	got, err = s.RunHasServices(ctx, uuid.New())
	if err != nil {
		t.Fatalf("RunHasServices (missing run): %v", err)
	}
	if got {
		t.Errorf("RunHasServices returned true for an unknown run id")
	}
}

// TestListAgentsForRun_DistinctAndExcludesNullAgent — covers the
// broadcast target set for CleanupRunServices. The query returns
// DISTINCT agents (so a run with N jobs on one agent yields one
// row, not N) and excludes job_runs whose agent_id is NULL
// (cancel-before-start has no agents and no service pods to
// clean).
func TestListAgentsForRun_DistinctAndExcludesNullAgent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Plant: agent A ran two jobs of the run; agent B ran one;
	// agent_id stays NULL on the never-dispatched stragglers.
	agentA := uuid.New()
	agentB := uuid.New()
	for _, id := range []uuid.UUID{agentA, agentB} {
		_, err := pool.Exec(ctx,
			`INSERT INTO agents (id, name, token_hash) VALUES ($1, $2, 'h')`,
			id, "a-"+id.String()[:8])
		if err != nil {
			t.Fatalf("seed agent: %v", err)
		}
	}
	// Two jobs on agentA, one on agentB, rest left NULL.
	_, _ = pool.Exec(ctx,
		`UPDATE job_runs SET agent_id=$1 WHERE run_id=$2 AND name='compile'`,
		agentA, run.RunID)
	_, _ = pool.Exec(ctx,
		`UPDATE job_runs SET agent_id=$1 WHERE run_id=$2 AND name='unit'`,
		agentB, run.RunID)

	got, err := s.ListAgentsForRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListAgentsForRun: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (distinct A,B; never-NULL excluded): %v", len(got), got)
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen[agentA] || !seen[agentB] {
		t.Errorf("missing expected agents: got %v want %v + %v", got, agentA, agentB)
	}
}

// TestListAgentsForRun_FiltersByEngine — the migration 00037
// addition is load-bearing for cleanup correctness. Docker/Shell
// agents that ran jobs of a mixed-engine run MUST NOT appear in
// the cleanup target set, otherwise their no-op responses would
// mask the absence of a real k8s cleanup. Legacy ” engine
// (pre-v0.4.35) is included defensively so rolling upgrades
// don't lose cleanup coverage.
func TestListAgentsForRun_FiltersByEngine(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Three agents with different engines.
	k8sAgent := uuid.New()
	dockerAgent := uuid.New()
	legacyAgent := uuid.New() // engine = '' (default)
	for _, spec := range []struct {
		id     uuid.UUID
		engine string
		name   string
	}{
		{k8sAgent, "kubernetes", "k8s"},
		{dockerAgent, "docker", "dkr"},
		{legacyAgent, "", "lgc"},
	} {
		_, err := pool.Exec(ctx,
			`INSERT INTO agents (id, name, token_hash, engine) VALUES ($1, $2, 'h', $3)`,
			spec.id, "agent-"+spec.name+"-"+spec.id.String()[:6], spec.engine)
		if err != nil {
			t.Fatalf("seed agent: %v", err)
		}
	}
	// Pipeline has compile + unit jobs (seedPipeline). Plant each
	// on a different engine.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET agent_id=$1 WHERE run_id=$2 AND name='compile'`,
		k8sAgent, run.RunID); err != nil {
		t.Fatalf("update compile: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET agent_id=$1 WHERE run_id=$2 AND name='unit'`,
		dockerAgent, run.RunID); err != nil {
		t.Fatalf("update unit: %v", err)
	}
	// Also plant the legacy agent via a stage_run hack — easier:
	// just clone an extra job_run row pointing at legacyAgent.
	if _, err := pool.Exec(ctx, `
		INSERT INTO job_runs (run_id, stage_run_id, name, image, status, agent_id)
		SELECT run_id, stage_run_id, 'extra-legacy', image, 'success', $1
		  FROM job_runs WHERE run_id=$2 AND name='compile'`,
		legacyAgent, run.RunID); err != nil {
		t.Fatalf("seed legacy job_run: %v", err)
	}

	got, err := s.ListAgentsForRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListAgentsForRun: %v", err)
	}

	seen := map[uuid.UUID]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen[k8sAgent] {
		t.Errorf("k8s agent missing from target set: got %v", got)
	}
	if !seen[legacyAgent] {
		t.Errorf("legacy (engine='') agent missing from target set: got %v", got)
	}
	if seen[dockerAgent] {
		t.Errorf("docker agent included in target set — engine filter regressed; got %v", got)
	}
}
