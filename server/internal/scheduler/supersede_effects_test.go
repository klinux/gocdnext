package scheduler_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

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
	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 1, 0)

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

	victim, _ := seed(t, pool) // counter 1
	var pipelineID uuid.UUID
	var victimCounter int64
	if err := pool.QueryRow(ctx, `SELECT pipeline_id, counter FROM runs WHERE id=$1`, victim).
		Scan(&pipelineID, &victimCounter); err != nil {
		t.Fatalf("victim row: %v", err)
	}
	// A newer superseding run + mark the victim superseded by it.
	var superseding uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (pipeline_id, counter, cause, revisions, ref) VALUES ($1,$2,'webhook','{}','main') RETURNING id`,
		pipelineID, victimCounter+1).Scan(&superseding); err != nil {
		t.Fatalf("insert superseding run: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='canceled', superseded_by=$2 WHERE id=$1`, victim, superseding); err != nil {
		t.Fatalf("mark superseded: %v", err)
	}

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

// A run with no running cancel-requested jobs (the common gate-pending victim)
// dispatches nothing — no spurious frames.
func TestFireSupersedeEffects_NoRunningJobsNoFrames(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 1, 0)

	sched.FireSupersedeEffects(ctx, runID) // compile is still 'queued', no cancel intent

	select {
	case msg := <-sess.Out():
		t.Fatalf("unexpected frame for an idle superseded run: %+v", msg)
	case <-time.After(200 * time.Millisecond):
		// no frame — correct
	}
}
