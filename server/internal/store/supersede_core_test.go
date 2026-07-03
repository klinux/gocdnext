package store

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// gateFixture applies a 5-stage gated deploy pipeline
//
//	build → gate-staging → deploy-staging(env=staging) → gate-prod → deploy-prod(env=prod)
//
// so gate-staging governs {staging} and gate-prod governs {prod}. Every run of it
// stamps BOTH gates awaiting_approval at creation (runs.go), so a fresh run's env
// set is {staging,prod}; "advancing past staging" (approveGate) drops the staging
// gate and narrows the set to {prod} — the exact state that makes a newer *staging*
// run leave an older *prod*-pending run alone.
type gateFixture struct {
	s          *Store
	pool       *pgxpool.Pool
	ctx        context.Context
	pipelineID uuid.UUID
	materialID uuid.UUID
	def        domain.Pipeline
}

func newGateFixture(t *testing.T, slug string) gateFixture {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := New(pool)
	ctx := context.Background()
	url := "https://github.com/acme/" + slug
	fp := domain.GitFingerprint(url, "main")
	def := domain.Pipeline{
		Name:      "p1",
		Supersede: domain.SupersedeBranch,
		Stages:    []string{"build", "gate-staging", "deploy-staging", "gate-prod", "deploy-prod"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
		}},
		Jobs: []domain.Job{
			{Name: "compile", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}},
			{Name: "approve-staging", Stage: "gate-staging", Approval: &domain.ApprovalSpec{Required: 1}},
			{Name: "dep-staging", Stage: "deploy-staging", Image: "alpine",
				Tasks: []domain.Task{{Script: "true"}}, Deploy: &domain.DeploySpec{Environment: "staging"}},
			{Name: "approve-prod", Stage: "gate-prod", Approval: &domain.ApprovalSpec{Required: 1}},
			{Name: "dep-prod", Stage: "deploy-prod", Image: "alpine",
				Tasks: []domain.Task{{Script: "true"}}, Deploy: &domain.DeploySpec{Environment: "prod"}},
		},
	}
	applied, err := s.ApplyProject(ctx, ApplyProjectInput{Slug: slug, Name: slug, Pipelines: []*domain.Pipeline{&def}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	return gateFixture{s, pool, ctx, applied.Pipelines[0].PipelineID, materialID, def}
}

func (f gateFixture) createRun(t *testing.T, branch string) RunCreated {
	t.Helper()
	run, err := f.s.CreateRunFromModification(f.ctx, CreateRunFromModificationInput{
		PipelineID: f.pipelineID, MaterialID: f.materialID, ModificationID: 1,
		Revision: "abc", Branch: branch, Provider: "github", Delivery: "d", TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run
}

// approveGate simulates an approval by flipping the gate job to 'success' so it
// stops being an awaiting_approval gate — the DB state production reaches once the
// gate is approved and the run advances past that stage.
func (f gateFixture) approveGate(t *testing.T, runID uuid.UUID, gate string) {
	t.Helper()
	ct, err := f.pool.Exec(f.ctx,
		`UPDATE job_runs SET status='success' WHERE run_id=$1 AND name=$2 AND status='awaiting_approval'`, runID, gate)
	if err != nil {
		t.Fatalf("approve gate: %v", err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("approve gate %q: expected 1 row, got %d (precondition: gate was awaiting)", gate, ct.RowsAffected())
	}
}

type runState struct {
	status       string
	supersededBy *uuid.UUID
	cancelReason *string
}

func (f gateFixture) stateOf(t *testing.T, id uuid.UUID) runState {
	t.Helper()
	var st runState
	var sb pgtype.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT status, superseded_by, cancel_reason FROM runs WHERE id=$1`, id,
	).Scan(&st.status, &sb, &st.cancelReason); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	if sb.Valid {
		u := fromPgUUID(sb)
		st.supersededBy = &u
	}
	return st
}

// runSupersedeE opens a tx, runs supersedeLaneSiblings for `newer` in branch mode,
// commits, and returns the victims or an error. Goroutine-safe (no t.Fatalf) so
// concurrent tests can funnel failures through a channel to the main goroutine.
func (f gateFixture) runSupersedeE(newer RunCreated, ref string, readyEnvs []string) ([]SupersededRun, error) {
	tx, err := f.pool.BeginTx(f.ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	out, err := f.s.supersedeLaneSiblings(f.ctx, tx, supersedeInput{
		PipelineID:   f.pipelineID,
		Ref:          ref,
		LaneMode:     domain.SupersedeBranch,
		NewerRunID:   newer.RunID,
		NewerCounter: newer.Counter,
		ReadyEnvs:    readyEnvs,
		Def:          f.def,
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(f.ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// runSupersede is the main-goroutine wrapper that fails the test on error.
func (f gateFixture) runSupersede(t *testing.T, newer RunCreated, ref string, readyEnvs []string) []SupersededRun {
	t.Helper()
	out, err := f.runSupersedeE(newer, ref, readyEnvs)
	if err != nil {
		t.Fatalf("supersedeLaneSiblings: %v", err)
	}
	return out
}

// A newer run clears the older pending pile in one lane: victims are returned +
// terminalized in counter-DESC order, stamped superseded_by the newer run, and the
// cancel_reason cites the counter ONLY (never the branch/ref value).
func TestSupersede_DESCAndReason(t *testing.T) {
	f := newGateFixture(t, "supdesc")
	r1 := f.createRun(t, "main")
	r2 := f.createRun(t, "main")
	r3 := f.createRun(t, "main")

	victims := f.runSupersede(t, r3, "main", []string{"staging"})

	if len(victims) != 2 {
		t.Fatalf("victims = %d, want 2", len(victims))
	}
	if victims[0].Counter != r2.Counter || victims[1].Counter != r1.Counter {
		t.Fatalf("victim order = [%d,%d], want DESC [%d,%d]",
			victims[0].Counter, victims[1].Counter, r2.Counter, r1.Counter)
	}
	for _, id := range []uuid.UUID{r1.RunID, r2.RunID} {
		st := f.stateOf(t, id)
		if st.status != "canceled" {
			t.Fatalf("run %s status = %q, want canceled", id, st.status)
		}
		if st.supersededBy == nil || *st.supersededBy != r3.RunID {
			t.Fatalf("run %s superseded_by = %v, want %s", id, st.supersededBy, r3.RunID)
		}
		if st.cancelReason == nil {
			t.Fatalf("run %s cancel_reason is nil", id)
		}
		// Audit motive is counter-only — never leaks the branch/ref value.
		wantReason := "superseded by #" + strconv.FormatInt(r3.Counter, 10)
		if *st.cancelReason != wantReason {
			t.Fatalf("run %s cancel_reason = %q, want %q", id, *st.cancelReason, wantReason)
		}
		if strings.Contains(*st.cancelReason, "main") {
			t.Fatalf("cancel_reason leaks the ref: %q", *st.cancelReason)
		}
	}
	if st := f.stateOf(t, r3.RunID); st.status != "queued" {
		t.Fatalf("newer run status = %q, want queued (untouched)", st.status)
	}
}

// A newer run whose ready gate governs STAGING must NOT cancel an older run that
// already passed staging and is pending only the PROD gate. The reverse (prod
// clears prod) must cancel.
func TestSupersede_EnvIntersectionStagingVsProd(t *testing.T) {
	f := newGateFixture(t, "supenv")
	victim := f.createRun(t, "main")
	f.approveGate(t, victim.RunID, "approve-staging") // now pending only prod → env set {prod}
	newer := f.createRun(t, "main")

	// staging-scoped supersede leaves the prod-pending victim alone.
	if got := f.runSupersede(t, newer, "main", []string{"staging"}); len(got) != 0 {
		t.Fatalf("staging supersede canceled %d prod-pending victims, want 0", len(got))
	}
	if st := f.stateOf(t, victim.RunID); st.status != "queued" || st.supersededBy != nil {
		t.Fatalf("prod-pending victim was touched by a staging supersede: %+v", st)
	}

	// prod-scoped supersede cancels it.
	got := f.runSupersede(t, newer, "main", []string{"prod"})
	if len(got) != 1 || got[0].Counter != victim.Counter {
		t.Fatalf("prod supersede victims = %+v, want [#%d]", got, victim.Counter)
	}
	if st := f.stateOf(t, victim.RunID); st.status != "canceled" {
		t.Fatalf("prod-pending victim status = %q, want canceled", st.status)
	}
}

// supersedeOne locks + revalidates each candidate; a candidate that stopped being
// supersedable between selection and the lock (its gate got decided, or the run
// went terminal) is skipped WITHOUT canceling it.
func TestSupersede_StaleRevalidation(t *testing.T) {
	f := newGateFixture(t, "supstale")
	envsForGate := func(g string) []string { return f.def.GovernedEnvs(g) }
	in := supersedeInput{
		PipelineID: f.pipelineID, Ref: "main", LaneMode: domain.SupersedeBranch,
		NewerRunID: uuid.New(), NewerCounter: 999, ReadyEnvs: []string{"staging"}, Def: f.def,
	}

	call := func(t *testing.T, id uuid.UUID, counter int64) *SupersededRun {
		t.Helper()
		tx, err := f.pool.BeginTx(f.ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		got, err := f.s.supersedeOne(f.ctx, tx, f.s.q.WithTx(tx), id, counter, in, envsForGate)
		if err != nil {
			t.Fatalf("supersedeOne: %v", err)
		}
		if err := tx.Commit(f.ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return got
	}

	t.Run("gate decided under us", func(t *testing.T) {
		v := f.createRun(t, "main")
		// Both gates approved → no awaiting gate remains: revalidation must skip it.
		f.approveGate(t, v.RunID, "approve-staging")
		f.approveGate(t, v.RunID, "approve-prod")
		if got := call(t, v.RunID, v.Counter); got != nil {
			t.Fatalf("supersedeOne canceled a run with no awaiting gate: %+v", got)
		}
		if st := f.stateOf(t, v.RunID); st.status != "queued" || st.supersededBy != nil {
			t.Fatalf("gate-decided run was terminalized: %+v", st)
		}
	})

	t.Run("run already terminal under us", func(t *testing.T) {
		v := f.createRun(t, "main")
		if _, err := f.pool.Exec(f.ctx,
			`UPDATE runs SET status='canceled', finished_at=NOW() WHERE id=$1`, v.RunID); err != nil {
			t.Fatalf("pre-cancel: %v", err)
		}
		if got := call(t, v.RunID, v.Counter); got != nil {
			t.Fatalf("supersedeOne re-terminalized an already-canceled run: %+v", got)
		}
		if st := f.stateOf(t, v.RunID); st.supersededBy != nil {
			t.Fatalf("already-terminal run got superseded_by stamped: %+v", st)
		}
	})
}

// Two newer runs racing to clear an overlapping pile must not deadlock: both
// process victims in counter-DESC order (one global descending order), so they
// serialize on the shared rows instead of cycling. Final state: the whole older
// pile ends canceled exactly once.
func TestSupersede_ConcurrentNoDeadlock(t *testing.T) {
	f := newGateFixture(t, "suprace")
	r1 := f.createRun(t, "main")
	r2 := f.createRun(t, "main")
	r3 := f.createRun(t, "main")
	r4 := f.createRun(t, "main")

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	// A supersedes for #3 (victims #2,#1); B supersedes for #4 (victims #3,#2,#1).
	go func() { defer wg.Done(); _, err := f.runSupersedeE(r3, "main", []string{"staging"}); errCh <- err }()
	go func() { defer wg.Done(); _, err := f.runSupersedeE(r4, "main", []string{"staging"}); errCh <- err }()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent supersede errored (possible deadlock/abort): %v", err)
		}
	}

	for _, r := range []RunCreated{r1, r2, r3} {
		if st := f.stateOf(t, r.RunID); st.status != "canceled" {
			t.Fatalf("run #%d status = %q, want canceled", r.Counter, st.status)
		}
	}
	if st := f.stateOf(t, r4.RunID); st.status != "queued" {
		t.Fatalf("newest run #%d status = %q, want queued (never a victim)", r4.Counter, st.status)
	}
}

// The HIGH: a queued victim job that the scheduler's AssignJob flips to running
// CONCURRENTLY with the supersede must still be terminalized — the post-cancel
// snapshot has to return it so the caller sends a CancelJob frame. AssignJob is a
// bare `status='queued'` CAS that never checks runs.status, so cancel-before-
// snapshot ordering is the only thing that closes the gap; snapshot-first would
// miss the job and leave it executing inside a canceled/superseded run.
func TestSupersede_RacingAssignJobStillCanceled(t *testing.T) {
	f := newGateFixture(t, "suprace3")
	victim := f.createRun(t, "main")
	newer := f.createRun(t, "main")

	var compileID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM job_runs WHERE run_id=$1 AND name='compile'`, victim.RunID).Scan(&compileID); err != nil {
		t.Fatalf("compile job id: %v", err)
	}
	agentID := uuid.New()

	// Scheduler wins AssignJob on the queued job but holds the row lock uncommitted,
	// mirroring the production UPDATE (dispatch.go / scheduler.sql AssignJob).
	txSched, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin sched tx: %v", err)
	}
	defer func() { _ = txSched.Rollback(f.ctx) }()
	var schedPID int
	if err := txSched.QueryRow(f.ctx, `SELECT pg_backend_pid()`).Scan(&schedPID); err != nil {
		t.Fatalf("sched backend pid: %v", err)
	}
	ct, err := txSched.Exec(f.ctx,
		`UPDATE job_runs SET status='running', agent_id=$2, started_at=NOW()
		 WHERE id=$1 AND status='queued' AND agent_id IS NULL`, compileID, agentID)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("assign affected %d rows, want 1", ct.RowsAffected())
	}

	// supersede runs concurrently; CancelQueuedJobsInRun blocks on the row lock
	// txSched holds, forcing the "AssignJob mid-terminalize" interleaving.
	type result struct {
		victims []SupersededRun
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		tx, e := f.pool.BeginTx(f.ctx, pgx.TxOptions{})
		if e != nil {
			ch <- result{err: e}
			return
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		out, e := f.s.supersedeLaneSiblings(f.ctx, tx, supersedeInput{
			PipelineID: f.pipelineID, Ref: "main", LaneMode: domain.SupersedeBranch,
			NewerRunID: newer.RunID, NewerCounter: newer.Counter,
			ReadyEnvs: []string{"staging"}, Def: f.def,
		})
		if e != nil {
			ch <- result{err: e}
			return
		}
		if e := tx.Commit(f.ctx); e != nil {
			ch <- result{err: e}
			return
		}
		ch <- result{victims: out}
	}()

	// Deterministic barrier: wait until the supersede backend is actually blocked
	// on txSched's row lock before we let AssignJob commit.
	waitUntilBlockedBy(t, f.pool, f.ctx, schedPID)

	// Let AssignJob win: compile is now committed-running.
	if err := txSched.Commit(f.ctx); err != nil {
		t.Fatalf("commit sched: %v", err)
	}

	r := <-ch
	if r.err != nil {
		t.Fatalf("supersede: %v", r.err)
	}
	if len(r.victims) != 1 {
		t.Fatalf("victims = %d, want 1", len(r.victims))
	}
	found := false
	for _, j := range r.victims[0].RunningJobs {
		if j.JobID == compileID {
			found = true
			if j.AgentID != agentID {
				t.Fatalf("running job agent = %s, want %s", j.AgentID, agentID)
			}
		}
	}
	if !found {
		t.Fatalf("racing AssignJob'd job %s missing from RunningJobs — would run inside a canceled run", compileID)
	}

	// The job stays running (AssignJob won the CAS) with cancel_requested_at
	// stamped; the caller's CancelJob frame + the agent's JobResult finalize it.
	var status string
	var reqAt *time.Time
	if err := f.pool.QueryRow(f.ctx,
		`SELECT status, cancel_requested_at FROM job_runs WHERE id=$1`, compileID).Scan(&status, &reqAt); err != nil {
		t.Fatalf("read compile: %v", err)
	}
	if status != "running" {
		t.Fatalf("compile status = %q, want running (AssignJob won the CAS)", status)
	}
	if reqAt == nil {
		t.Fatalf("compile cancel_requested_at not stamped — caller can't finalize the cancel")
	}
	if st := f.stateOf(t, victim.RunID); st.status != "canceled" || st.supersededBy == nil {
		t.Fatalf("victim run state = %+v, want canceled + superseded_by", st)
	}
}

// The MED: an in-flight approval on the victim's gate must NOT deadlock supersede
// or let it cancel based on a decision-in-flight. Supersede holds runs(V) then
// cancels gate/job rows (runs → job_runs); the approval cascade holds the gate row
// then wants runs(V) (job_runs → runs). The bounded lock_timeout makes supersede
// abort its savepoint and leave the victim pending — the approval owns the decision.
func TestSupersede_InFlightApprovalBails(t *testing.T) {
	f := newGateFixture(t, "supappr")
	victim := f.createRun(t, "main")
	newer := f.createRun(t, "main")

	var gateID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM job_runs WHERE run_id=$1 AND name='approve-staging'`, victim.RunID).Scan(&gateID); err != nil {
		t.Fatalf("gate id: %v", err)
	}

	// Simulate an approval mid-cascade: hold the gate row lock uncommitted, exactly
	// as decideGate's terminal UPDATE would (approvals.go).
	txAppr, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin approval tx: %v", err)
	}
	defer func() { _ = txAppr.Rollback(f.ctx) }()
	ct, err := txAppr.Exec(f.ctx,
		`UPDATE job_runs SET status='success', decided_at=NOW() WHERE id=$1 AND status='awaiting_approval'`, gateID)
	if err != nil {
		t.Fatalf("approval flip: %v", err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("approval flip affected %d rows, want 1", ct.RowsAffected())
	}

	type result struct {
		victims []SupersededRun
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		out, e := f.runSupersedeE(newer, "main", []string{"staging"})
		ch <- result{out, e}
	}()

	// Supersede must return promptly (bounded lock_timeout ~75ms), NOT hang on the
	// gate row txAppr holds. A generous ceiling still catches an unbounded wait.
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("supersede errored instead of bailing the contended victim: %v", r.err)
		}
		if len(r.victims) != 0 {
			t.Fatalf("supersede canceled %d victims while an approval was in flight, want 0", len(r.victims))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supersede hung on the in-flight gate row — lock_timeout bail not working")
	}

	// Victim untouched: the approval owns the decision; the flip was rolled back.
	if st := f.stateOf(t, victim.RunID); st.status != "queued" || st.supersededBy != nil {
		t.Fatalf("contended victim was terminalized: %+v", st)
	}
	_ = txAppr.Rollback(f.ctx)
}

// The short lock_timeout supersede sets must be scoped to the pass: after
// supersedeLaneSiblings returns, the SAME outer tx must see its original timeout so
// later writes (fire points share the tx) don't inherit 55P03-on-contention.
func TestSupersede_LockTimeoutRestoredOnTx(t *testing.T) {
	f := newGateFixture(t, "suptimeout")
	newer := f.createRun(t, "main") // lone run → zero candidates, fast path

	tx, err := f.pool.BeginTx(f.ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()

	if _, err := f.s.supersedeLaneSiblings(f.ctx, tx, supersedeInput{
		PipelineID: f.pipelineID, Ref: "main", LaneMode: domain.SupersedeBranch,
		NewerRunID: newer.RunID, NewerCounter: newer.Counter,
		ReadyEnvs: []string{"staging"}, Def: f.def,
	}); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	var lt string
	if err := tx.QueryRow(f.ctx, `SELECT current_setting('lock_timeout')`).Scan(&lt); err != nil {
		t.Fatalf("read lock_timeout: %v", err)
	}
	if lt != "0" {
		t.Fatalf("lock_timeout leaked to outer tx: %q, want 0 (restored)", lt)
	}
}

// The most delicate bail: the lock timeout fires AFTER partial terminalization —
// the run flip + queued-stage/job cancels have run, then StampCancelRequestedAtForRun
// blocks on a running job row another tx holds. The per-victim savepoint must roll
// back ALL of it: run stays queued, the gate stays awaiting, nothing superseded.
func TestSupersede_TimeoutMidTerminalizeRollsBack(t *testing.T) {
	f := newGateFixture(t, "suprollback")
	victim := f.createRun(t, "main")
	newer := f.createRun(t, "main")

	var compileID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM job_runs WHERE run_id=$1 AND name='compile'`, victim.RunID).Scan(&compileID); err != nil {
		t.Fatalf("compile id: %v", err)
	}
	// Put the victim's compile into 'running' (committed) so StampCancelRequestedAtForRun
	// has a row to lock during terminalization.
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE job_runs SET status='running', agent_id=$2, started_at=NOW() WHERE id=$1`,
		compileID, uuid.New()); err != nil {
		t.Fatalf("set running: %v", err)
	}

	// Hold the running compile row so the terminalizer blocks at the stamp step —
	// AFTER it has already flipped the run + canceled the awaiting gate in its savepoint.
	txHold, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin hold tx: %v", err)
	}
	defer func() { _ = txHold.Rollback(f.ctx) }()
	var held uuid.UUID
	if err := txHold.QueryRow(f.ctx,
		`SELECT id FROM job_runs WHERE id=$1 FOR UPDATE`, compileID).Scan(&held); err != nil {
		t.Fatalf("hold compile row: %v", err)
	}

	type result struct {
		victims []SupersededRun
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		out, e := f.runSupersedeE(newer, "main", []string{"staging"})
		ch <- result{out, e}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("supersede errored instead of bailing: %v", r.err)
		}
		if len(r.victims) != 0 {
			t.Fatalf("victims = %d, want 0 (rolled back)", len(r.victims))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supersede hung mid-terminalize — savepoint bail not working")
	}

	// Everything the savepoint touched must be undone.
	if st := f.stateOf(t, victim.RunID); st.status != "queued" || st.supersededBy != nil {
		t.Fatalf("run not rolled back: %+v", st)
	}
	var gateStatus string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT status FROM job_runs WHERE run_id=$1 AND name='approve-staging'`, victim.RunID).Scan(&gateStatus); err != nil {
		t.Fatalf("gate status: %v", err)
	}
	if gateStatus != "awaiting_approval" {
		t.Fatalf("gate status = %q, want awaiting_approval (cancel rolled back)", gateStatus)
	}
	_ = txHold.Rollback(f.ctx)
}

// waitUntilBlockedBy polls until some backend is waiting on a lock held
// specifically by blockerPID (the txSched backend), so the barrier can't be
// satisfied by an unrelated waiter.
func waitUntilBlockedBy(t *testing.T, pool *pgxpool.Pool, ctx context.Context, blockerPID int) {
	t.Helper()
	for i := 0; i < 250; i++ {
		var blocked bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`,
			blockerPID,
		).Scan(&blocked); err != nil {
			t.Fatalf("poll pg_blocking_pids: %v", err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a backend to block on pid %d's row lock", blockerPID)
}
