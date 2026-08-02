package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func isDeadlock(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "40P01") || strings.Contains(err.Error(), "deadlock"))
}

// terminalizeJobAndStage forces a job + its stage_run to a terminal status via
// raw SQL, so a concurrent test can make ANOTHER job the LAST pending work in the
// run — the only way the run row itself becomes contended (its close/cancel).
func terminalizeJobAndStage(t *testing.T, pool *pgxpool.Pool, jobID, stageRunID uuid.UUID, status string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status=$2, finished_at=NOW() WHERE id=$1`, jobID, status); err != nil {
		t.Fatalf("terminalize job: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status=$2, finished_at=NOW() WHERE id=$1`, stageRunID, status); err != nil {
		t.Fatalf("terminalize stage: %v", err)
	}
}

// waitJobLocked blocks until `jobID` is row-locked by ANOTHER transaction —
// detected by a `SELECT ... FOR UPDATE NOWAIT` failing with 55P03. It is the
// deterministic barrier the rollback test needs: it proves CancelRun has entered
// its tx and locked the job (past the pre-check) before we release the run lock.
func waitJobLocked(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := pool.Exec(ctx, `SELECT 1 FROM job_runs WHERE id=$1 FOR UPDATE NOWAIT`, jobID)
		if err != nil && strings.Contains(err.Error(), "55P03") {
			return // locked by CancelRun
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for CancelRun to lock the job")
}

// #207 Part 2a: CancelRun stamps running jobs (cancel_origin=user_run, left
// running for the agent to finalise), flips queued/awaiting to canceled, cancels
// the run, and fans out a CancelJob frame ONLY to agent-owned running rows.
func TestCancelRun_StampsRunningCancelsQueuedFanout(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, compile, unit := seedRunningJob(t, pool) // compile running+agent, unit queued

	res, err := s.CancelRun(ctx, runID)
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	if got := scalarStr(t, pool, `SELECT status FROM runs WHERE id=$1`, runID); got != "canceled" {
		t.Errorf("run status = %q, want canceled", got)
	}
	// Running job: stays running, stamped, origin user_run.
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, compile); got != "running" {
		t.Errorf("running job status = %q, want running (agent finalises it)", got)
	}
	var stamp *string
	if err := pool.QueryRow(ctx, `SELECT cancel_requested_at::text FROM job_runs WHERE id=$1`, compile).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if stamp == nil {
		t.Error("running job cancel_requested_at not stamped")
	}
	if got := scalarStr(t, pool, `SELECT cancel_origin FROM job_runs WHERE id=$1`, compile); got != "user_run" {
		t.Errorf("running job cancel_origin = %q, want user_run", got)
	}
	// Queued job: flipped to canceled, origin user_run.
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, unit); got != "canceled" {
		t.Errorf("queued job status = %q, want canceled", got)
	}
	if got := scalarStr(t, pool, `SELECT cancel_origin FROM job_runs WHERE id=$1`, unit); got != "user_run" {
		t.Errorf("queued job cancel_origin = %q, want user_run", got)
	}
	// Fanout: exactly the agent-owned running job.
	if len(res.RunningJobs) != 1 || res.RunningJobs[0].JobID != compile {
		t.Fatalf("RunningJobs = %+v, want [compile]", res.RunningJobs)
	}

	// The stamped running job then completes: the agent reports failed, the CASE
	// records canceled.
	agentID, attempt := jobAgentAttempt(t, pool, compile)
	if _, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: compile, Status: "failed", ExitCode: 1,
		ExpectedAgentID: agentID, ExpectedAttempt: attempt,
	}); err != nil || !ok {
		t.Fatalf("complete stamped job: ok=%v err=%v", ok, err)
	}
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, compile); got != "canceled" {
		t.Errorf("stamped job final status = %q, want canceled", got)
	}
}

// A native running job (agent_id NULL) is STAMPED but NOT fanned out (no frame to
// send) — the watcher/reaper drives it.
func TestCancelRun_NativeRunningStampedNotFannedOut(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, _, unit := seedRunningJob(t, pool)
	// Make unit a server-managed native running job (agent_id NULL).
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='running', agent_id=NULL, started_at=NOW() WHERE id=$1`, unit); err != nil {
		t.Fatalf("native flip: %v", err)
	}

	res, err := s.CancelRun(ctx, runID)
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	// Native job stamped (cancel intent recorded) but not in the fanout.
	var stamp *string
	if err := pool.QueryRow(ctx, `SELECT cancel_requested_at::text FROM job_runs WHERE id=$1`, unit).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if stamp == nil {
		t.Error("native running job not stamped (agent_id filter must be gone)")
	}
	for _, r := range res.RunningJobs {
		if r.JobID == unit {
			t.Error("native (agent_id NULL) job must NOT be in the CancelJob fanout")
		}
	}
}

// The final run CAS losing rolls the WHOLE tx back (409, no partial cancel). A
// concurrent tx terminalizes the run to success and holds the row lock; CancelRun
// blocks on CancelActiveRun, then sees success → 0 rows → ErrRunAlreadyTerminal,
// and the queued job it had already flipped is rolled back to queued.
func TestCancelRun_LosesRunCAS_RollsBackFully(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, stageBuild, _, compile, unit := seedRunningJob(t, pool)
	// Make `unit` (queued) the only thing keeping the run open, so CancelRun's
	// rollback is observable on it.
	terminalizeJobAndStage(t, pool, compile, stageBuild, "success")

	// tx2 terminalizes the run and HOLDS its row lock (uncommitted), so CancelRun's
	// CancelActiveRun will block on it.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	if _, err := tx2.Exec(ctx, `UPDATE runs SET status='success', finished_at=NOW() WHERE id=$1`, runID); err != nil {
		t.Fatalf("tx2 terminalize: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.CancelRun(ctx, runID)
		done <- err
	}()

	// Deterministic barrier: only release the run lock AFTER CancelRun has entered
	// its tx and locked the job (past the pre-check). Then CancelActiveRun sees the
	// now-committed success → 0 rows → ErrRunAlreadyTerminal → full rollback.
	waitJobLocked(t, pool, unit)
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("tx2 commit: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, store.ErrRunAlreadyTerminal) {
			t.Fatalf("CancelRun = %v, want ErrRunAlreadyTerminal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CancelRun did not return — likely stuck on the run lock")
	}

	// Full rollback: the queued job CancelRun had already flipped is back to queued,
	// and never got a cancel stamp/origin.
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, unit); got != "queued" {
		t.Errorf("queued job status = %q, want queued (cancel tx rolled back)", got)
	}
	if got := scalarStr(t, pool, `SELECT COALESCE(cancel_origin,'') FROM job_runs WHERE id=$1`, unit); got != "" {
		t.Errorf("queued job cancel_origin = %q, want empty (rolled back)", got)
	}
}

// Concurrent last-job completion × CancelRun must never deadlock (40P01). CancelRun
// locks jobs → stages → run, matching completion's job → stage → run.
func TestCancelRun_ConcurrentWithCompletion_NoDeadlock(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, stageTest, compile, unit := seedRunningJob(t, pool)
	// Make `compile` the LAST pending job: terminalize the test stage so completing
	// compile actually CLOSES the run (updates the run row) — the only way this
	// races CancelRun on the run row. Without this the run stays open and neither op
	// touches it, so the test couldn't tell job→stage→run from run→job.
	terminalizeJobAndStage(t, pool, unit, stageTest, "success")
	agentID, attempt := jobAgentAttempt(t, pool, compile)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, errs[0] = s.CompleteJob(ctx, store.CompleteJobInput{
			JobRunID: compile, Status: "success", ExitCode: 0,
			ExpectedAgentID: agentID, ExpectedAttempt: attempt,
		})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = s.CancelRun(ctx, runID)
	}()

	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent completion × cancel timed out (possible deadlock/stuck lock)")
	}
	for i, err := range errs {
		if isDeadlock(err) {
			t.Fatalf("op %d deadlocked (40P01): %v", i, err)
		}
	}
	// Whoever won, the run is terminal (canceled if CancelRun won, success if the
	// completion closed it first) and compile is terminal — never stuck.
	if got := scalarStr(t, pool, `SELECT status FROM runs WHERE id=$1`, runID); got != "canceled" && got != "success" {
		t.Errorf("run status = %q, want a terminal state (canceled|success)", got)
	}
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, compile); got != "canceled" && got != "success" {
		t.Errorf("compile status = %q, want terminal", got)
	}
}

// #207 (HIGH: revival must clear old cancel state). Full cycle: an upstream
// failure SYSTEM-cancels a downstream (origin=dependency); rerunning the upstream
// revives it AND clears its cancel_origin; the user then deliberately cancels the
// revived downstream (origin=user_job); a second upstream rerun must NOT resurrect
// it. Without clearing cancel_origin on revive, the COALESCE would keep
// 'dependency' and the second rerun would wrongly revive the user's cancel.
func TestRerunJob_RevivalClearsOriginThenUserCancelSticks(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// compile (build stage) running, unit (test stage) queued.
	runID, _, _, compile, unit := seedRunningJob(t, pool)
	_ = runID

	// 1) compile (running) fails → fail-fast cancels the downstream unit
	// (origin=dependency). CompleteJob is the running-job terminalizer;
	// FailJobWithReason only matches queued rows.
	cAgent, cAttempt := jobAgentAttempt(t, pool, compile)
	if _, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: compile, Status: "failed", ExitCode: 1,
		ExpectedAgentID: cAgent, ExpectedAttempt: cAttempt,
	}); err != nil || !ok {
		t.Fatalf("fail compile: ok=%v err=%v", ok, err)
	}
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, unit); got != "canceled" {
		t.Fatalf("unit status = %q, want canceled (fail-fast)", got)
	}
	if got := scalarStr(t, pool, `SELECT COALESCE(cancel_origin,'') FROM job_runs WHERE id=$1`, unit); got != "dependency" {
		t.Fatalf("unit cancel_origin = %q, want dependency", got)
	}

	// Simulate a downstream that ALSO carried a cancel_requested_at stamp (e.g. a
	// run-cancel had stamped it while running) so we can prove the revive clears the
	// TIMESTAMP too, not just the origin.
	if _, err := pool.Exec(ctx, `UPDATE job_runs SET cancel_requested_at = NOW() WHERE id=$1`, unit); err != nil {
		t.Fatalf("pre-stamp unit: %v", err)
	}

	// 2) rerun compile → unit revived to queued, origin AND timestamp CLEARED.
	if _, err := s.RerunJob(ctx, store.RerunJobInput{JobRunID: compile, TriggeredBy: "user:test"}); err != nil {
		t.Fatalf("rerun compile #1: %v", err)
	}
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, unit); got != "queued" {
		t.Fatalf("unit status = %q, want queued (revived)", got)
	}
	if got := scalarStr(t, pool, `SELECT COALESCE(cancel_origin,'') FROM job_runs WHERE id=$1`, unit); got != "" {
		t.Fatalf("unit cancel_origin = %q, want cleared on revive", got)
	}
	var revivedStamp *string
	if err := pool.QueryRow(ctx, `SELECT cancel_requested_at::text FROM job_runs WHERE id=$1`, unit).Scan(&revivedStamp); err != nil {
		t.Fatal(err)
	}
	if revivedStamp != nil {
		t.Fatalf("unit cancel_requested_at = %v, want NULL (cleared on revive)", *revivedStamp)
	}

	// 3) user deliberately cancels the revived (queued) unit → origin=user_job.
	if _, err := s.CancelJobRun(ctx, unit); err != nil {
		t.Fatalf("cancel unit: %v", err)
	}
	if got := scalarStr(t, pool, `SELECT COALESCE(cancel_origin,'') FROM job_runs WHERE id=$1`, unit); got != "user_job" {
		t.Fatalf("unit cancel_origin = %q, want user_job", got)
	}

	// 4) make compile terminal again, then rerun it — unit must NOT be resurrected.
	if _, ok, err := s.FailJobWithReason(ctx, compile, "boom2"); err != nil || !ok {
		t.Fatalf("fail compile #2: ok=%v err=%v", ok, err)
	}
	if _, err := s.RerunJob(ctx, store.RerunJobInput{JobRunID: compile, TriggeredBy: "user:test"}); err != nil {
		t.Fatalf("rerun compile #2: %v", err)
	}
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, unit); got != "canceled" {
		t.Errorf("unit status = %q, want canceled (a user_job cancel is NOT revived)", got)
	}
}

// #207 Part 2c: UnassignJob is two-outcome. With NO cancel stamped it re-queues
// (covered by the sweeper tests). With a cancel stamped in the AssignJob→Dispatch
// window it TERMINALISES 'canceled', KEEPS cancel_requested_at + cancel_origin, and
// cascades the stage IN THE SAME TX — never resurrecting the job into 'queued'.
func TestUnassignJob_CanceledOutcomeTerminalizesAndCascades(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, stageBuild, _, compile, _ := seedRunningJob(t, pool)
	agentID, attempt := jobAgentAttempt(t, pool, compile)
	// A cancel landed after AssignJob but before Dispatch: stamp intent + origin.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET cancel_requested_at=NOW(), cancel_origin='user_job' WHERE id=$1`, compile); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	runID, outcome, ok, err := s.UnassignJob(ctx, compile, agentID, attempt)
	if err != nil || !ok {
		t.Fatalf("UnassignJob: ok=%v err=%v", ok, err)
	}
	if outcome != "canceled" {
		t.Fatalf("outcome = %q, want canceled (cancel raced dispatch)", outcome)
	}
	if runID == uuid.Nil {
		t.Fatal("nil run id")
	}

	// Job terminalised canceled, NOT resurrected to queued; stamp+origin preserved.
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, compile); got != "canceled" {
		t.Errorf("status = %q, want canceled", got)
	}
	if got := scalarStr(t, pool, `SELECT COALESCE(cancel_origin,'') FROM job_runs WHERE id=$1`, compile); got != "user_job" {
		t.Errorf("cancel_origin = %q, want user_job (preserved)", got)
	}
	var stamp *string
	if err := pool.QueryRow(ctx, `SELECT cancel_requested_at::text FROM job_runs WHERE id=$1`, compile).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if stamp == nil {
		t.Error("cancel_requested_at must be preserved on the canceled outcome")
	}
	// Cascade ran in the same tx: the build stage (only compile) derived canceled.
	if got := scalarStr(t, pool, `SELECT status FROM stage_runs WHERE id=$1`, stageBuild); got != "canceled" {
		t.Errorf("build stage status = %q, want canceled (in-tx cascade)", got)
	}
}

// Concurrent queued single-job cancel × run cancel must never deadlock.
func TestCancelRun_ConcurrentWithQueuedJobCancel_NoDeadlock(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, stageBuild, _, compile, unit := seedRunningJob(t, pool)
	// Make `unit` (queued) the LAST pending job so its single-job cancel CLOSES the
	// run (touches the run row), contending with CancelRun on it.
	terminalizeJobAndStage(t, pool, compile, stageBuild, "success")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, errs[0] = s.CancelJobRun(ctx, unit) }()
	go func() { defer wg.Done(); _, errs[1] = s.CancelRun(ctx, runID) }()

	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent queued-cancel × run-cancel timed out")
	}
	for i, err := range errs {
		if isDeadlock(err) {
			t.Fatalf("op %d deadlocked (40P01): %v", i, err)
		}
	}
}
