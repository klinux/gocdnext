package store

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

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

// runSupersede opens a tx, runs supersedeLaneSiblings for `newer` in branch mode,
// commits, and returns the terminalized victims.
func (f gateFixture) runSupersede(t *testing.T, newer RunCreated, ref string, readyEnvs []string) []SupersededRun {
	t.Helper()
	tx, err := f.pool.BeginTx(f.ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
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
		t.Fatalf("supersedeLaneSiblings: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatalf("commit: %v", err)
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
	wg.Add(2)
	// A supersedes for #3 (victims #2,#1); B supersedes for #4 (victims #3,#2,#1).
	go func() { defer wg.Done(); f.runSupersede(t, r3, "main", []string{"staging"}) }()
	go func() { defer wg.Done(); f.runSupersede(t, r4, "main", []string{"staging"}) }()
	wg.Wait()

	for _, r := range []RunCreated{r1, r2, r3} {
		if st := f.stateOf(t, r.RunID); st.status != "canceled" {
			t.Fatalf("run #%d status = %q, want canceled", r.Counter, st.status)
		}
	}
	if st := f.stateOf(t, r4.RunID); st.status != "queued" {
		t.Fatalf("newest run #%d status = %q, want queued (never a victim)", r4.Counter, st.status)
	}
}
