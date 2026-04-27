package scheduler_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const testDSN = ""

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seed creates a project + 1 pipeline (stages build/test, jobs compile/unit)
// and queues one run against it. Returns the run id + the material id the
// revisions snapshot refers to (for JobAssignment assertions).
func seed(t *testing.T, pool *pgxpool.Pool) (runID, materialID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()

	fp := domain.GitFingerprint("https://github.com/org/demo", "main")
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"build", "test"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/demo", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "compile", Stage: "build", Image: "golang:1.23", Tasks: []domain.Task{{Script: "make"}}},
				{Name: "unit", Stage: "test", Image: "golang:1.23", Tasks: []domain.Task{{Script: "make test"}}, Needs: []string{"compile"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID

	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("mat lookup: %v", err)
	}

	runRes, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:  pipelineID,
		MaterialID:  materialID,
		Revision:    "abc0123456789abc0123456789abc0123456789a",
		Branch:      "main",
		Provider:    "github",
		Delivery:    "test",
		TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return runRes.RunID, materialID
}

// seedAgentRow creates an `agents` row so AssignJob's FK to agent_id holds.
// The SessionStore is in-memory, but the DB still needs the agent to exist.
func seedAgentRow(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO agents (name, token_hash) VALUES ($1, 'hash') RETURNING id`, name,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return id
}

func TestDispatchRun_PushesAssignmentToIdleAgent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, materialID := seed(t, pool)
	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 1)

	sched.DispatchRun(ctx, runID)

	select {
	case msg := <-sess.Out():
		assign := msg.GetAssign()
		if assign == nil {
			t.Fatalf("message is not JobAssignment: %+v", msg)
		}
		if assign.Name != "compile" {
			t.Fatalf("job name = %q, want compile", assign.Name)
		}
		if assign.RunId != runID.String() {
			t.Fatalf("run_id = %s, want %s", assign.RunId, runID)
		}
		if len(assign.Checkouts) != 1 {
			t.Fatalf("checkouts len = %d, want 1", len(assign.Checkouts))
		}
		co := assign.Checkouts[0]
		if co.MaterialId != materialID.String() || co.Revision == "" {
			t.Fatalf("checkout = %+v", co)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no assignment delivered within 2s")
	}

	var status, runStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE name='compile'`).Scan(&status)
	_ = pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus)
	if status != "running" || runStatus != "running" {
		t.Fatalf("job=%s run=%s, want running/running", status, runStatus)
	}
}

func TestDispatchRun_NoIdleAgentKeepsJobQueued(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	sched.DispatchRun(ctx, runID)

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE name='compile'`).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %s, want queued", status)
	}
}

func TestDispatchRun_SkipsSecondStageJobs(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 4)

	sched.DispatchRun(ctx, runID)

	// Drain whatever was sent; there must be exactly 1 assignment (compile)
	// — the unit job sits in stage 2 and is blocked by the active-stage gate.
	count := 0
drain:
	for {
		select {
		case msg, ok := <-sess.Out():
			if !ok {
				break drain
			}
			if msg.GetAssign() != nil {
				count++
			}
		case <-time.After(200 * time.Millisecond):
			break drain
		}
	}
	if count != 1 {
		t.Fatalf("dispatched %d assignments, want 1 (stage gate)", count)
	}
}

func TestDispatchRun_RoutesToAgentMatchingTags(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	// Pipeline with a single job that requires the `docker` tag.
	url, branch := "https://github.com/org/tagged", "main"
	fp := domain.GitFingerprint(url, branch)
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "tagged", Name: "Tagged",
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "build", Stage: "build", Tasks: []domain.Task{{Script: "docker build ."}}, Tags: []string{"docker"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var matID uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&matID)

	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applyRes.Pipelines[0].PipelineID, MaterialID: matID,
		Revision: "abc0123456789abc0123456789abc0123456789a", Branch: "main",
		Provider: "github", Delivery: "t", TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Two agents: plain (no docker) and dockerized.
	plainID := seedAgentRow(t, pool, "plain")
	dockerID := seedAgentRow(t, pool, "dockerized")
	plainSess := sessions.CreateSession(plainID, []string{"linux"}, 1)
	dockerSess := sessions.CreateSession(dockerID, []string{"linux", "docker"}, 1)

	sched.DispatchRun(ctx, run.RunID)

	// The plain agent must not have received anything.
	select {
	case msg := <-plainSess.Out():
		if msg.GetAssign() != nil {
			t.Fatalf("plain agent received an assignment despite lacking docker tag: %+v", msg.GetAssign())
		}
	case <-time.After(200 * time.Millisecond):
		// ok
	}

	// The docker agent must have it.
	select {
	case msg := <-dockerSess.Out():
		if msg.GetAssign() == nil {
			t.Fatalf("docker agent got non-assignment message: %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("docker agent never received the assignment")
	}
}

func TestDispatchRun_LeavesJobQueuedWhenNoAgentHasRequiredTags(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	url, branch := "https://github.com/org/gpuonly", "main"
	fp := domain.GitFingerprint(url, branch)
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "gpuonly", Name: "GPU Only",
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"train"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "train", Stage: "train", Tasks: []domain.Task{{Script: "train.sh"}}, Tags: []string{"gpu"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var matID uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&matID)

	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applyRes.Pipelines[0].PipelineID, MaterialID: matID,
		Revision: "abc0123456789abc0123456789abc0123456789a", Branch: "main",
		Provider: "github", Delivery: "t", TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	plainID := seedAgentRow(t, pool, "plain-only")
	sessions.CreateSession(plainID, []string{"linux"}, 1)

	sched.DispatchRun(ctx, run.RunID)

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE name='train'`).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %q, want queued (no gpu agent available)", status)
	}
}

func TestBuildAssignment_InjectsSecretsIntoEnvAndMasks(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"deploy"},
		Jobs: []domain.Job{
			{
				Name: "push", Stage: "deploy",
				Tasks:   []domain.Task{{Script: "echo $GH_TOKEN"}},
				Secrets: []string{"GH_TOKEN", "REGISTRY_PASSWORD"},
			},
		},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "push"}

	secrets := map[string]string{
		"GH_TOKEN":          "ghp_abc123",
		"REGISTRY_PASSWORD": "reg-pw-xyz",
	}
	got, err := scheduler.BuildAssignment(run, job, nil, secrets, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.Env["GH_TOKEN"] != "ghp_abc123" {
		t.Fatalf("env[GH_TOKEN] = %q", got.Env["GH_TOKEN"])
	}
	if got.Env["REGISTRY_PASSWORD"] != "reg-pw-xyz" {
		t.Fatalf("env[REGISTRY_PASSWORD] = %q", got.Env["REGISTRY_PASSWORD"])
	}
	// LogMasks must carry the VALUES, not the names — the runner matches on
	// the raw string to redact it from stdout/stderr.
	masks := map[string]bool{}
	for _, m := range got.LogMasks {
		masks[m] = true
	}
	if !masks["ghp_abc123"] || !masks["reg-pw-xyz"] {
		t.Fatalf("log_masks missing values: %+v", got.LogMasks)
	}
}

func TestBuildAssignment_PropagatesProfileAndResources(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"build"},
		Jobs: []domain.Job{{
			Name: "build", Stage: "build",
			Profile: "gpu",
			Resources: domain.ResourceSpec{
				Requests: domain.ResourceQuantities{CPU: "500m", Memory: "512Mi"},
				Limits:   domain.ResourceQuantities{CPU: "2", Memory: "2Gi"},
			},
			Tasks: []domain.Task{{Script: "make"}},
		}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "build"}

	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.GetProfile() != "gpu" {
		t.Fatalf("profile = %q, want gpu", got.GetProfile())
	}
	r := got.GetResources()
	if r == nil {
		t.Fatal("expected resources, got nil")
	}
	if r.GetCpuRequest() != "500m" || r.GetMemoryRequest() != "512Mi" {
		t.Fatalf("requests = %+v", r)
	}
	if r.GetCpuLimit() != "2" || r.GetMemoryLimit() != "2Gi" {
		t.Fatalf("limits = %+v", r)
	}
}

func TestBuildAssignment_NoResourcesLeavesProtoNil(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"x"},
		Jobs:   []domain.Job{{Name: "j", Stage: "x", Tasks: []domain.Task{{Script: "echo"}}}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "j"}

	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.GetResources() != nil {
		t.Fatalf("expected nil resources, got %+v", got.GetResources())
	}
	if got.GetProfile() != "" {
		t.Fatalf("expected empty profile, got %q", got.GetProfile())
	}
}

func TestBuildAssignment_MissingSecretIsError(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"x"},
		Jobs:   []domain.Job{{Name: "j", Stage: "x", Secrets: []string{"ABSENT"}}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "j"}

	if _, err := scheduler.BuildAssignment(run, job, nil, map[string]string{}, nil); err == nil {
		t.Fatalf("expected error when declared secret is unresolved")
	}
}

func TestJobSecretsFromDefinition_Extracts(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"s"},
		Jobs: []domain.Job{
			{Name: "a", Stage: "s", Secrets: []string{"X"}},
			{Name: "b", Stage: "s"},
		},
	}
	defJSON, _ := json.Marshal(def)

	got, err := scheduler.JobSecretsFromDefinition(defJSON, "a")
	if err != nil || len(got) != 1 || got[0] != "X" {
		t.Fatalf("secrets for a = %v err=%v", got, err)
	}
	got, err = scheduler.JobSecretsFromDefinition(defJSON, "b")
	if err != nil || len(got) != 0 {
		t.Fatalf("secrets for b = %v err=%v", got, err)
	}
	if _, err := scheduler.JobSecretsFromDefinition(defJSON, "nope"); err == nil {
		t.Fatalf("expected error for unknown job name")
	}
}

func TestBuildAssignment_MapsTasksAndCheckouts(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"build"},
		Jobs: []domain.Job{
			{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "echo hi"}}},
		},
		Variables: map[string]string{"FOO": "bar"},
	}
	defJSON, _ := json.Marshal(def)
	materialID := uuid.New()

	run := store.RunForDispatch{
		ID:         uuid.New(),
		PipelineID: uuid.New(),
		Definition: defJSON,
		Revisions: json.RawMessage(`{"` + materialID.String() +
			`":{"revision":"deadbeef","branch":"main"}}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "compile", Image: "golang:1.23"}
	gitCfg, _ := json.Marshal(domain.GitMaterial{URL: "https://github.com/x/y", Branch: "main"})
	materials := []store.Material{{
		ID: materialID, Type: string(domain.MaterialGit), Config: gitCfg,
	}}

	got, err := scheduler.BuildAssignment(run, job, materials, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.Name != "compile" || got.Image != "golang:1.23" {
		t.Fatalf("%+v", got)
	}
	if len(got.Tasks) != 1 || got.Tasks[0].GetScript() != "echo hi" {
		t.Fatalf("tasks = %+v", got.Tasks)
	}
	if got.Env["FOO"] != "bar" {
		t.Fatalf("env = %+v", got.Env)
	}
	if len(got.Checkouts) != 1 || got.Checkouts[0].Revision != "deadbeef" {
		t.Fatalf("checkouts = %+v", got.Checkouts)
	}
}

func TestBuildAssignment_PropagatesPipelineServices(t *testing.T) {
	// Pipeline-level services must ride through the assignment so
	// the agent can stand them up on a job-scoped docker network
	// before tasks run. Empty services → nil on the wire (the
	// agent's fast path that skips network plumbing entirely).
	def := domain.Pipeline{
		Stages: []string{"test"},
		Jobs: []domain.Job{
			{Name: "integration", Stage: "test", Tasks: []domain.Task{{Script: "go test"}}},
		},
		Services: []domain.Service{
			{Name: "postgres", Image: "postgres:16-alpine",
				Env: map[string]string{"POSTGRES_PASSWORD": "test"}},
			{Name: "cache", Image: "redis:7",
				Command: []string{"redis-server", "--appendonly", "no"}},
		},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "integration", Image: "golang:1.25"}

	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if len(got.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(got.Services))
	}
	if got.Services[0].Name != "postgres" ||
		got.Services[0].Image != "postgres:16-alpine" ||
		got.Services[0].Env["POSTGRES_PASSWORD"] != "test" {
		t.Errorf("postgres spec = %+v", got.Services[0])
	}
	if got.Services[1].Name != "cache" ||
		len(got.Services[1].Command) != 3 ||
		got.Services[1].Command[0] != "redis-server" {
		t.Errorf("cache spec = %+v", got.Services[1])
	}
}

func TestBuildAssignment_NoServicesWhenPipelineHasNone(t *testing.T) {
	// Fast path: len(def.Services)==0 → nil on the wire so engines
	// that skip service plumbing don't pay an empty-slice penalty.
	def := domain.Pipeline{
		Stages: []string{"build"},
		Jobs:   []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "compile"}
	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.Services != nil {
		t.Errorf("services should be nil when empty, got %+v", got.Services)
	}
}

// TestRun_DispatchesOnAgentRegister exercises the SessionStore hook:
// a run sits queued because no agent was online when it was created,
// then an agent registers and the scheduler dispatches it without
// waiting for the next periodic tick. The tick is set long here so
// a failure mode where the fix only works via the tick fallback
// would time out.
func TestRun_DispatchesOnAgentRegister(t *testing.T) {
	pool := dbtest.SetupPool(t)
	dsn := dbtest.DSN()
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), dsn).
		WithTickInterval(30 * time.Second) // too long to save the test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Seed a run BEFORE any agent is registered.
	seed(t, pool)

	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()

	// Let the scheduler finish its priming drainQueued — it'll find
	// the queued run but no agent, log "leaving queued", and wait.
	time.Sleep(300 * time.Millisecond)

	// Now bring an agent online; the ready-hook should kick the
	// scheduler into an immediate drainQueued.
	agentID := seedAgentRow(t, pool, "late-agent")
	sess := sessions.CreateSession(agentID, nil, 1)

	select {
	case msg := <-sess.Out():
		if msg.GetAssign() == nil {
			t.Fatalf("unexpected message: %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no assignment after agent register within 2s")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("scheduler did not stop after ctx cancel")
	}
}

// TestRun_ReactsToNotify exercises the LISTEN loop: NOTIFY fires, scheduler
// picks up the run, dispatches. Uses the dbtest DSN so the LISTEN connection
// sees commits from the same cluster.
func TestRun_ReactsToNotify(t *testing.T) {
	pool := dbtest.SetupPool(t)
	dsn := dbtest.DSN()
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), dsn).WithTickInterval(500 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 1)

	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()

	// Give the scheduler a moment to LISTEN before we fire the NOTIFY via
	// a fresh run (CreateRunFromModification emits it).
	time.Sleep(200 * time.Millisecond)
	seed(t, pool)

	select {
	case msg := <-sess.Out():
		if msg.GetAssign() == nil {
			t.Fatalf("unexpected message: %+v", msg)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("no assignment after NOTIFY within 4s")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("scheduler did not stop after ctx cancel")
	}
}

// TestDispatchRun_SerialPipelineWaitsForBusyRun exercises the
// concurrency: serial gate. An earlier run is parked in 'running'
// and a fresh one is created for the same pipeline — the scheduler
// must leave it queued instead of dispatching its jobs in parallel.
func TestDispatchRun_SerialPipelineWaitsForBusyRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	fp := domain.GitFingerprint("https://github.com/org/demo", "main")
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{{
			Name:        "deploy",
			Stages:      []string{"deploy"},
			Concurrency: domain.ConcurrencySerial,
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{
					URL: "https://github.com/org/demo", Branch: "main",
					Events: []string{"push"},
				},
			}},
			Jobs: []domain.Job{{
				Name: "apply", Stage: "deploy", Image: "alpine:3.19",
				Tasks: []domain.Task{{Script: "echo deploy"}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID

	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("mat lookup: %v", err)
	}

	// Run #1: mark it running by hand — stand-in for "the previous
	// trigger is still executing".
	first, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:  pipelineID,
		MaterialID:  materialID,
		Revision:    "aaa0123456789aaa0123456789aaa0123456789a",
		Branch:      "main",
		Provider:    "github",
		Delivery:    "t-1",
		TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := s.MarkRunRunning(ctx, first.RunID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// Run #2: should queue behind the running one.
	second, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:  pipelineID,
		MaterialID:  materialID,
		Revision:    "bbb0123456789bbb0123456789bbb0123456789b",
		Branch:      "main",
		Provider:    "github",
		Delivery:    "t-2",
		TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	agentID := seedAgentRow(t, pool, "serial-agent")
	sess := sessions.CreateSession(agentID, nil, 2)

	sched.DispatchRun(ctx, second.RunID)

	select {
	case msg := <-sess.Out():
		t.Fatalf("expected no dispatch while busy, got: %+v", msg)
	case <-time.After(200 * time.Millisecond):
		// good — stayed queued
	}

	// Unblock: mark first as finished. Dispatching again should
	// now deliver the job to the agent without further wait.
	if _, err := pool.Exec(ctx, `UPDATE runs SET status='success', finished_at=NOW() WHERE id=$1`, first.RunID); err != nil {
		t.Fatalf("finish first: %v", err)
	}
	sched.DispatchRun(ctx, second.RunID)
	select {
	case msg := <-sess.Out():
		if msg.GetAssign() == nil {
			t.Fatalf("unexpected message: %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no dispatch after previous run finished")
	}
}
