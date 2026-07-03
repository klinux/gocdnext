package scheduler_test

import (
	"context"
	"testing"
	"time"

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
