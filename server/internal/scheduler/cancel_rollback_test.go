package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedUndispatchedDeploy sets up the exact state rollbackUndispatchedAssignment
// faces: a deploy `ship` job flipped running, with an in_progress deployment
// revision created (as the dispatch path does) just before the frame failed.
func seedUndispatchedDeploy(t *testing.T, pool *pgxpool.Pool, s *store.Store, slug string) (runID, jobID, agentID uuid.UUID, attempt int32, revID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	_, older, _ := seedDeployRuns(t, pool, slug, domain.SupersedeOff)
	runID = older.RunID
	jobID = soleJobID(t, older)
	agentID = seedAgentRow(t, pool, slug+"-agent")
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW() WHERE id=$2`, agentID, jobID); err != nil {
		t.Fatalf("flip running: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT attempt FROM job_runs WHERE id=$1`, jobID).Scan(&attempt); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	projectID := projectIDForSlug(t, pool, slug)
	envID, err := s.EnsureEnvironment(ctx, projectID, "prod")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	revID, err = s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: attempt, Version: "v1",
	})
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	return
}

// withRunQueuedListen LISTENs on the run_queued channel, runs fn, and reports
// whether a NOTIFY arrived — the observable proof of NotifyRunQueued.
func withRunQueuedListen(t *testing.T, pool *pgxpool.Pool, fn func()) bool {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+store.RunQueuedChannel); err != nil {
		t.Fatalf("listen: %v", err)
	}
	fn()
	wctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(wctx)
	return err == nil && n != nil
}

func revisionCount(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM deployment_revisions WHERE job_run_id=$1`, jobID).Scan(&n); err != nil {
		t.Fatalf("revision count: %v", err)
	}
	return n
}

// #207: the QUEUED outcome deletes the pre-dispatch revision (no deploy happened),
// re-queues the job, and FIRES NotifyRunQueued.
func TestRollbackUndispatched_QueuedOutcome_DeletesRevisionAndNotifies(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sched := scheduler.New(s, grpcsrv.NewSessionStore(), quietLogger(), testDSN)
	ctx := context.Background()

	runID, jobID, agentID, attempt, revID := seedUndispatchedDeploy(t, pool, s, "rollback-queued")

	notified := withRunQueuedListen(t, pool, func() {
		scheduler.RollbackUndispatchedAssignmentForTest(sched, ctx, runID, jobID, agentID, attempt, revID)
	})

	if n := revisionCount(t, pool, jobID); n != 0 {
		t.Errorf("revision count = %d, want 0 (deleted before requeue)", n)
	}
	if st := jobStatusOf(t, pool, jobID); st != "queued" {
		t.Errorf("job status = %q, want queued", st)
	}
	if !notified {
		t.Error("queued outcome must NotifyRunQueued")
	}
}

// #207: the CANCELED outcome (a cancel raced the dispatch) deletes the revision and
// terminalises the job canceled — and must NOT NotifyRunQueued (nothing to dispatch).
func TestRollbackUndispatched_CanceledOutcome_DeletesRevisionNoNotify(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sched := scheduler.New(s, grpcsrv.NewSessionStore(), quietLogger(), testDSN)
	ctx := context.Background()

	runID, jobID, agentID, attempt, revID := seedUndispatchedDeploy(t, pool, s, "rollback-canceled")
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at=NOW(), cancel_origin='user_job' WHERE id=$1`, jobID); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	notified := withRunQueuedListen(t, pool, func() {
		scheduler.RollbackUndispatchedAssignmentForTest(sched, ctx, runID, jobID, agentID, attempt, revID)
	})

	if n := revisionCount(t, pool, jobID); n != 0 {
		t.Errorf("revision count = %d, want 0 (no deploy happened)", n)
	}
	if st := jobStatusOf(t, pool, jobID); st != "canceled" {
		t.Errorf("job status = %q, want canceled (cancel raced dispatch)", st)
	}
	if notified {
		t.Error("canceled outcome must NOT NotifyRunQueued")
	}
}
