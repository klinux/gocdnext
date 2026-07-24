package scheduler_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// markSuperseded inserts a newer superseding run and flips `victim` to
// canceled+superseded_by it — the DB state fireSupersedeEffects requires to claim
// (the effects only fire for actually-superseded runs). Returns the superseding id.
func markSuperseded(t *testing.T, pool *pgxpool.Pool, victim uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var pipelineID uuid.UUID
	var counter int64
	if err := pool.QueryRow(ctx, `SELECT pipeline_id, counter FROM runs WHERE id=$1`, victim).
		Scan(&pipelineID, &counter); err != nil {
		t.Fatalf("victim row: %v", err)
	}
	var superseding uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (pipeline_id, counter, cause, revisions, ref) VALUES ($1,$2,'webhook','{}','main') RETURNING id`,
		pipelineID, counter+1).Scan(&superseding); err != nil {
		t.Fatalf("insert superseding run: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='canceled', finished_at=NOW(), superseded_by=$2 WHERE id=$1`, victim, superseding); err != nil {
		t.Fatalf("mark superseded: %v", err)
	}
	return superseding
}

// spyChecks satisfies the scheduler's checksReporter (structurally — the interface
// is unexported) so the test can assert the supersede-cancel closes the check.
type spyChecks struct {
	mu        sync.Mutex
	completed []string // "runID:status"
}

func (s *spyChecks) ReportRunCompleted(_ context.Context, runID uuid.UUID, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed = append(s.completed, runID.String()+":"+status)
}

// FireSupersedeEffects pushes a CancelJob frame to the agent running a job of a
// superseded run so the container stops promptly (the store already stamped
// cancel_requested_at; this is the prompt path the run_superseded NOTIFY drives).
func TestFireSupersedeEffects_PushesCancelToRunningJob(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	markSuperseded(t, pool, runID)
	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 1, 0)
	markReady(t, sessions, sess.ID)

	// Reproduce the post-supersede DB state: the run's compile job is running on
	// the agent with a stamped cancel intent.
	var jobID string
	if err := pool.QueryRow(ctx,
		`UPDATE job_runs SET status='running', agent_id=$2, started_at=NOW(), cancel_requested_at=NOW()
		 WHERE run_id=$1 AND name='compile' RETURNING id`, runID, agentID).Scan(&jobID); err != nil {
		t.Fatalf("stage running job: %v", err)
	}

	sched.FireSupersedeEffects(ctx, runID)

	select {
	case msg := <-sess.Out():
		c := msg.GetCancel()
		if c == nil {
			t.Fatalf("message is not CancelJob: %+v", msg)
		}
		if c.RunId != runID.String() || c.JobId != jobID {
			t.Fatalf("cancel frame = {run:%s job:%s}, want {run:%s job:%s}", c.RunId, c.JobId, runID, jobID)
		}
		if c.Reason != "superseded" {
			t.Fatalf("cancel reason = %q, want superseded", c.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no CancelJob frame delivered within 2s")
	}
}

// The effects listener emits the run.superseded audit (unified for both fire
// points) off the victim's superseded_by — counters + superseding id only, system
// actor, no branch/ref.
func TestFireSupersedeEffects_EmitsAudit(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	victim, _ := seed(t, pool)
	markSuperseded(t, pool, victim)

	sched.FireSupersedeEffects(ctx, victim)

	var actorID *uuid.UUID
	var metaRaw []byte
	if err := pool.QueryRow(ctx,
		`SELECT actor_id, metadata FROM audit_events WHERE target_id=$1 AND action='run.superseded'`,
		victim.String()).Scan(&actorID, &metaRaw); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if actorID != nil {
		t.Fatalf("run.superseded actor_id = %v, want NULL (system)", actorID)
	}
	if !strings.Contains(string(metaRaw), "by_counter") || strings.Contains(string(metaRaw), "main") {
		t.Fatalf("audit metadata wrong (missing counters or leaks ref): %s", metaRaw)
	}
}

// A supersede-cancel closes the run's GitHub check (the JobResult completion path
// that normally reports it is skipped on supersede — HIGH).
func TestFireSupersedeEffects_ClosesGithubCheck(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	spy := &spyChecks{}
	sched := scheduler.New(s, sessions, quietLogger(), testDSN).WithChecksReporter(spy)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	markSuperseded(t, pool, runID)
	sched.FireSupersedeEffects(ctx, runID)

	spy.mu.Lock()
	defer spy.mu.Unlock()
	want := runID.String() + ":canceled"
	if len(spy.completed) != 1 || spy.completed[0] != want {
		t.Fatalf("checks.ReportRunCompleted calls = %v, want [%s]", spy.completed, want)
	}
}

// pt.5d durability: the claim makes effects fire exactly once — a second
// FireSupersedeEffects (a replica's NOTIFY, or the replay) is a no-op — and once
// done the run drops off the replay work-list.
func TestFireSupersedeEffects_ClaimOnceThenDone(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	spy := &spyChecks{}
	sched := scheduler.New(s, sessions, quietLogger(), testDSN).WithChecksReporter(spy)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	markSuperseded(t, pool, runID)

	// Before firing, the run is on the replay work-list.
	pending, err := s.ListPendingSupersedeEffects(ctx, 100)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if !containsRun(pending, runID) {
		t.Fatalf("run should be pending effects before firing")
	}

	sched.FireSupersedeEffects(ctx, runID)
	sched.FireSupersedeEffects(ctx, runID) // second (replica/replay) must no-op

	spy.mu.Lock()
	n := len(spy.completed)
	spy.mu.Unlock()
	if n != 1 {
		t.Fatalf("check closed %d times, want exactly 1 (claim not idempotent)", n)
	}

	// Effects done → no longer on the replay work-list.
	pending, err = s.ListPendingSupersedeEffects(ctx, 100)
	if err != nil {
		t.Fatalf("list pending after: %v", err)
	}
	if containsRun(pending, runID) {
		t.Fatalf("run still pending effects after completion — MarkSupersedeEffectsDone didn't land")
	}
}

// The pt.5d durability promise (review MED): a superseded run WITH services but no
// k8s agent connected yet must NOT be marked done — cleanup couldn't reach a
// receiver, so the replay has to retry (once an agent reconnects) rather than leak
// the pods forever.
func TestFireSupersedeEffects_ServicesNoTargetStaysPending(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	markSuperseded(t, pool, runID)
	if _, err := pool.Exec(ctx, `UPDATE runs SET has_services=true WHERE id=$1`, runID); err != nil {
		t.Fatalf("set has_services: %v", err)
	}

	sched.FireSupersedeEffects(ctx, runID) // no k8s agent connected → cleanup can't resolve

	var doneAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT supersede_effects_at FROM runs WHERE id=$1`, runID).Scan(&doneAt); err != nil {
		t.Fatalf("read effects_at: %v", err)
	}
	if doneAt != nil {
		t.Fatalf("effects marked done with services + no cleanup target — would leak pods with no retry")
	}
}

func containsRun(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// A run with no running cancel-requested jobs (the common gate-pending victim)
// dispatches nothing — no spurious frames.
func TestFireSupersedeEffects_NoRunningJobsNoFrames(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	markSuperseded(t, pool, runID)
	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 1, 0)
	markReady(t, sessions, sess.ID)

	sched.FireSupersedeEffects(ctx, runID) // compile is still 'queued', no cancel intent

	select {
	case msg := <-sess.Out():
		t.Fatalf("unexpected frame for an idle superseded run: %+v", msg)
	case <-time.After(200 * time.Millisecond):
		// no frame — correct
	}
}
