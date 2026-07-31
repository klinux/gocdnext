package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedGovernedDeployPipeline applies a pipeline whose approval gate GOVERNS one or
// more deploy environments — the shape the freeze check actually cares about (the
// existing approval fixtures use a pure-approval gate, which governs nothing).
func seedGovernedDeployPipeline(t *testing.T, pool *pgxpool.Pool, slug string, envs []string, supersede string) (runID, gateJobID, projectID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	url, branch := "https://github.com/org/"+slug, "main"
	fp := store.FingerprintFor(url, branch)

	jobs := []domain.Job{{
		Name: "gate", Stage: "approve",
		Approval: &domain.ApprovalSpec{Description: "Ship it?"},
	}}
	for _, env := range envs {
		jobs = append(jobs, domain.Job{
			Name: "ship-" + env, Stage: "deploy", Image: "alpine:3.19",
			Tasks:  []domain.Task{{Script: "true"}},
			Deploy: &domain.DeploySpec{Environment: env, Version: "v1"},
		})
	}
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "release", Supersede: supersede, Stages: []string{"approve", "deploy"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: jobs,
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	projectID = res.ProjectID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: res.Pipelines[0].PipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "deadbeef", Branch: branch, Provider: "github",
		Delivery: slug, TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	runID = run.RunID
	for _, j := range run.JobRuns {
		if j.Name == "gate" {
			gateJobID = j.ID
		}
	}
	if gateJobID == uuid.Nil {
		t.Fatal("no gate job in the seeded run")
	}
	return runID, gateJobID, projectID
}

// approvalDecision builds a vote from a REAL user row: job_run_approvals.user_id
// is a foreign key, so a synthetic uuid would fail the insert on every path that
// gets far enough to record a vote (i.e. exactly the not-refused cases).
func approvalDecision(t *testing.T, pool *pgxpool.Pool, jobRunID uuid.UUID) store.ApprovalDecision {
	t.Helper()
	uid := seedUser(t, pool, "alice-"+jobRunID.String()[:8]+"@acme.io", "Alice")
	return store.ApprovalDecision{
		JobRunID: jobRunID, UserID: uid,
		User: "Alice", UserEmail: "alice@acme.io",
	}
}

func TestApproveGate_RefusedWhileAGovernedEnvIsFrozen(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, gateID, projectID := seedGovernedDeployPipeline(t, pool, "appr-freeze", []string{"production"}, domain.SupersedeOff)
	if _, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	_, err := s.ApproveGate(ctx, approvalDecision(t, pool, gateID))
	if !errors.Is(err, store.ErrEnvironmentFrozen) {
		t.Fatalf("err = %v, want ErrEnvironmentFrozen", err)
	}
	// The gate is untouched: a refused approval must not consume the decision
	// or leave an orphan vote behind.
	var status string
	var votes int
	if err := pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, gateID).Scan(&status); err != nil {
		t.Fatalf("gate status: %v", err)
	}
	if status != "awaiting_approval" {
		t.Fatalf("gate status = %q, want awaiting_approval", status)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM job_run_approvals WHERE job_run_id=$1`, gateID).Scan(&votes); err != nil {
		t.Fatalf("votes: %v", err)
	}
	if votes != 0 {
		t.Fatalf("votes = %d, want 0 (the refusal precedes the vote)", votes)
	}
}

func TestApproveGate_ListsEveryFrozenGovernedEnv(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, gateID, projectID := seedGovernedDeployPipeline(t, pool, "appr-multi",
		[]string{"production", "staging"}, domain.SupersedeOff)
	for _, env := range []string{"production", "staging"} {
		if _, err := s.FreezeEnvironment(ctx, projectID, env, testActor(), "close"); err != nil {
			t.Fatalf("freeze %s: %v", env, err)
		}
	}
	_, err := s.ApproveGate(ctx, approvalDecision(t, pool, gateID))
	if !errors.Is(err, store.ErrEnvironmentFrozen) {
		t.Fatalf("err = %v, want ErrEnvironmentFrozen", err)
	}
	// BOTH names, so the operator doesn't unfreeze one, retry, and get refused
	// again by the next.
	if !strings.Contains(err.Error(), "production") || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("err = %q, want it to name every frozen governed env", err)
	}
}

func TestApproveGate_OneOfManyFrozenStillBlocks(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, gateID, projectID := seedGovernedDeployPipeline(t, pool, "appr-one-of",
		[]string{"production", "staging"}, domain.SupersedeOff)
	if _, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	_, err := s.ApproveGate(ctx, approvalDecision(t, pool, gateID))
	if !errors.Is(err, store.ErrEnvironmentFrozen) {
		t.Fatalf("err = %v, want the approval blocked by the single frozen env", err)
	}
}

func TestRejectGate_SucceedsDuringAFreeze(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, gateID, projectID := seedGovernedDeployPipeline(t, pool, "appr-reject",
		[]string{"production"}, domain.SupersedeOff)
	if _, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	// A rejection cannot promote anything, so it takes no env/freeze locks and
	// is never refused: "stop this from shipping" is exactly the decision you
	// still want available during a change-freeze.
	if _, err := s.RejectGate(ctx, approvalDecision(t, pool, gateID)); err != nil {
		t.Fatalf("RejectGate during a freeze: %v", err)
	}
	var status, decision string
	if err := pool.QueryRow(ctx,
		`SELECT status, COALESCE(decision,'') FROM job_runs WHERE id=$1`, gateID).Scan(&status, &decision); err != nil {
		t.Fatalf("gate row: %v", err)
	}
	if status != "failed" || decision != "rejected" {
		t.Fatalf("gate = %q/%q, want failed/rejected", status, decision)
	}
}

func TestApproveGate_PureApprovalGateUnaffectedByFreeze(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// A gate governing NO deploy has no governed envs, so a freeze anywhere in
	// the project is irrelevant to it.
	pipelineID, materialID := seedApprovalPipeline(t, pool, "appr-pure", nil)
	_, gateID := triggerApprovalRun(t, pool, pipelineID, materialID)
	var projectID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT project_id FROM pipelines WHERE id=$1`, pipelineID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if _, err := s.ApproveGate(ctx, approvalDecision(t, pool, gateID)); err != nil {
		t.Fatalf("pure-approval gate refused during an unrelated freeze: %v", err)
	}
}

func TestApproveGate_ThawedEnvApprovesNormally(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, gateID, projectID := seedGovernedDeployPipeline(t, pool, "appr-thaw",
		[]string{"production"}, domain.SupersedeBranch)
	if _, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	// One decision reused across both attempts — the same approver retrying
	// after the thaw, which is the real-world sequence.
	decision := approvalDecision(t, pool, gateID)
	if _, err := s.ApproveGate(ctx, decision); !errors.Is(err, store.ErrEnvironmentFrozen) {
		t.Fatalf("err = %v, want ErrEnvironmentFrozen", err)
	}
	if _, err := s.UnfreezeEnvironment(ctx, projectID, "production", testActor()); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	// Supersede=branch here on purpose: the approve path then also takes the
	// LANE lock before the freeze lock and re-takes it reentrantly inside
	// writeGatePassMarkers. A mismatched lane key or a wrong lock order would
	// hang here rather than return.
	if _, err := s.ApproveGate(ctx, decision); err != nil {
		t.Fatalf("approve after thaw: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, gateID).Scan(&status); err != nil {
		t.Fatalf("gate status: %v", err)
	}
	if status != "success" {
		t.Fatalf("gate status = %q, want success", status)
	}
}

// The "always q.WithTx(tx)" rule made a DETECTABLE regression.
//
// The approve path holds advisory locks across its whole transaction. Any read
// that reaches for a SECOND pooled connection while holding them deadlocks — and
// at MaxConns=1 it deadlocks DETERMINISTICALLY rather than under load in
// production. So the test runs the real approval against a one-connection pool:
// if someone later swaps a `q.WithTx(tx)` back to `s.q`, this hangs and fails on
// the context deadline instead of shipping.
func TestApproveGate_HoldsNoSecondConnection(t *testing.T) {
	shared := dbtest.SetupPool(t)
	_, gateID, projectID := seedGovernedDeployPipeline(t, shared, "appr-maxconns",
		[]string{"production"}, domain.SupersedeBranch)
	decision := approvalDecision(t, shared, gateID)

	cfg, err := pgxpool.ParseConfig(dbtest.DSN())
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("single-conn pool: %v", err)
	}
	defer single.Close()
	s := store.New(single)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Frozen first: exercises the freeze query itself on the tx-bound handle.
	if _, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close"); err != nil {
		t.Fatalf("freeze on a single-conn pool: %v", err)
	}
	if _, err := s.ApproveGate(ctx, decision); !errors.Is(err, store.ErrEnvironmentFrozen) {
		t.Fatalf("approve = %v, want ErrEnvironmentFrozen (a hang here means a second connection was acquired)", err)
	}
	if _, err := s.UnfreezeEnvironment(ctx, projectID, "production", testActor()); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	// And the full approve path (lane locks + gate-pass markers + cascade).
	if _, err := s.ApproveGate(ctx, decision); err != nil {
		t.Fatalf("approve on a single-conn pool: %v", err)
	}
}

// The load-bearing ordering claim, exercised with the REAL dispatch-side lane
// lock held.
//
// The earlier version of this test called AssignDeployJobIfEnvNotFrozen
// directly, which takes ONLY the freeze lock — the lane lock is the caller's
// (BeginDeploymentRevisionGuard, a SESSION-level lock on its own connection).
// With no lane lock held on the dispatch side, an approval that took
// freeze-then-lane could never deadlock, so the test would have passed on
// exactly the regression it exists to catch.
//
// Here the sequence is forced:
//
//  1. dispatch acquires the lane guard (session lock) and holds it
//  2. approval starts; correct order makes it block on the LANE lock while
//     holding nothing, inverted order makes it take the FREEZE lock and then
//     block on the lane
//  3. dispatch asks for the freeze lock
//
// Correct order: dispatch gets the freeze lock, commits, releases the lane, and
// the approval proceeds. Inverted order: dispatch waits on freeze while approval
// waits on lane — a cycle Postgres cannot report, because the lane side is a
// session lock on another connection, so it surfaces as a hang rather than an
// error. The context deadline turns that hang into a failure.
func TestApprovalAndDispatchLockOrder_NoDeadlock(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	runID, gateID, projectID := seedGovernedDeployPipeline(t, pool, "lock-order",
		[]string{"production"}, domain.SupersedeBranch)
	decision := approvalDecision(t, pool, gateID)

	var jobID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM job_runs WHERE run_id=$1 AND name='ship-production'`, runID).Scan(&jobID); err != nil {
		t.Fatalf("deploy job: %v", err)
	}
	var agentID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO agents (name, token_hash) VALUES ('lock-order-agent','h') RETURNING id`).Scan(&agentID); err != nil {
		t.Fatalf("agent: %v", err)
	}
	var pipelineID uuid.UUID
	var counter int64
	var ref string
	if err := pool.QueryRow(context.Background(),
		`SELECT pipeline_id, counter, ref FROM runs WHERE id=$1`, runID).Scan(&pipelineID, &counter, &ref); err != nil {
		t.Fatalf("run row: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// (1) Dispatch takes the lane lock — the same key writeGatePassMarkers and
	// the approve path compute, via the same helper.
	guard, status, err := s.BeginDeploymentRevisionGuard(ctx, store.BeginDeploymentRevisionGuardInput{
		PipelineID: pipelineID, Counter: counter, Ref: ref,
		LaneMode: domain.SupersedeBranch, Environment: "production",
	})
	if err != nil {
		t.Fatalf("deployment guard: %v", err)
	}
	if status != store.DeploymentRevisionGuardAllowed || guard == nil {
		t.Fatalf("guard status = %q, want allowed with a held lock", status)
	}
	laneReleased := false
	defer func() {
		if !laneReleased {
			_ = guard.Release(context.Background())
		}
	}()

	// (2) Approval runs concurrently. Under the correct order it parks on the
	// lane lock; under the inverted order it grabs the freeze lock first.
	approveDone := make(chan error, 1)
	go func() {
		_, aerr := s.ApproveGate(ctx, decision)
		approveDone <- aerr
	}()

	// Barrier on REAL state, not a sleep: block until the approval is actually
	// parked on the lane lock. A timed sleep is what makes this kind of test
	// silently vacuous — under -race or a loaded CI box the approval may not
	// have reached its first lock yet, the admission then sails through, the
	// lane is released, and an INVERTED implementation passes.
	//
	// Both orderings converge on "waiting for the lane key", which is what makes
	// it a valid barrier for either: the correct one waits there holding
	// nothing, the inverted one waits there already holding the freeze lock —
	// which is precisely the state that must deadlock the admission below.
	waitForAdvisoryLockWaiter(t, ctx, pool,
		store.LaneEnvLockKey(pipelineID, domain.SupersedeBranch, ref, "production"))

	// (3) Dispatch now wants the freeze lock, while still holding the lane lock.
	admitDone := make(chan error, 1)
	go func() {
		_, _, aerr := s.AssignDeployJobIfEnvNotFrozen(ctx, jobID, agentID, projectID, "production")
		admitDone <- aerr
	}()

	select {
	case err := <-admitDone:
		if err != nil {
			t.Fatalf("admission: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("admission never completed: dispatch holds the lane lock and is stuck on the freeze lock — " +
			"the approval path must take the LANE lock before the FREEZE lock")
	}

	// Release the lane so the approval can finish.
	if err := guard.Release(context.Background()); err != nil {
		t.Fatalf("release lane guard: %v", err)
	}
	laneReleased = true

	select {
	case err := <-approveDone:
		if err != nil && !errors.Is(err, store.ErrEnvironmentFrozen) && !errors.Is(err, store.ErrApprovalNotPending) {
			t.Fatalf("approve: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("approval never completed after the lane lock was released")
	}
}

// A freeze committing WHILE an approval is in flight is serialised by the freeze
// lock, and the ordering contract holds: an approval that commits before the
// freeze succeeds, one that arrives after is refused. Neither hangs.
func TestApprovalAndFreezeConcurrent_NoDeadlock(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	_, gateID, projectID := seedGovernedDeployPipeline(t, pool, "appr-order",
		[]string{"production"}, domain.SupersedeBranch)
	decision := approvalDecision(t, pool, gateID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan error, 2)
	start := make(chan struct{})
	go func() {
		<-start
		_, err := s.ApproveGate(ctx, decision)
		done <- err
	}()
	go func() {
		<-start
		_, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close")
		done <- err
	}()
	close(start)

	for i := range 2 {
		select {
		case err := <-done:
			// A frozen approval is a legitimate outcome of this race; a HANG is not.
			if err != nil && !errors.Is(err, store.ErrEnvironmentFrozen) {
				t.Fatalf("goroutine %d: %v", i, err)
			}
		case <-ctx.Done():
			t.Fatal("timed out — approval and freeze deadlocked")
		}
	}

	// Whatever the interleaving, the freeze is in force now, so the NEXT
	// approval attempt is refused.
	if _, found, _ := s.EnvironmentFrozenState(ctx, projectID, "production"); !found {
		t.Fatal("the environment is not frozen after FreezeEnvironment returned")
	}
}

// waitForAdvisoryLockWaiter blocks until some backend is WAITING on the given
// advisory-lock key (pg_locks.granted = false).
//
// pg_advisory_xact_lock(bigint) splits its key across (classid, objid) as the
// high and low 32 bits, with objsubid = 1 marking the single-argument form.
func waitForAdvisoryLockWaiter(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key int64) {
	t.Helper()
	u := uint64(key)
	classid, objid := int64(u>>32), int64(u&0xffffffff)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks
				WHERE locktype = 'advisory' AND NOT granted
				  AND classid::bigint = $1 AND objid::bigint = $2 AND objsubid = 1
			)`, classid, objid).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for a backend to block on the lane advisory lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
