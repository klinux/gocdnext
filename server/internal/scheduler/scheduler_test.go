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

	got, err := scheduler.BuildAssignment(run, job, materials)
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
