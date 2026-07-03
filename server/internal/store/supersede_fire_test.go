package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// newStage0GateFixture applies a GATE-FIRST pipeline
//
//	approve-staging(gate) → deploy-staging(staging) → approve-prod(gate) → deploy-prod(prod)
//
// so the staging gate sits at stage 0 and is READY the instant a run is created —
// the exact shape the creation supersede fire targets. `mode` is the supersede
// setting (off | branch | pipeline).
func newStage0GateFixture(t *testing.T, slug, mode string) gateFixture {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := New(pool)
	ctx := context.Background()
	url := "https://github.com/acme/" + slug
	fp := domain.GitFingerprint(url, "main")
	def := domain.Pipeline{
		Name:      "p1",
		Supersede: mode,
		Stages:    []string{"approve-staging", "deploy-staging", "approve-prod", "deploy-prod"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
		}},
		Jobs: []domain.Job{
			{Name: "approve-staging", Stage: "approve-staging", Approval: &domain.ApprovalSpec{Required: 1}},
			{Name: "dep-staging", Stage: "deploy-staging", Image: "alpine",
				Tasks: []domain.Task{{Script: "true"}}, Deploy: &domain.DeploySpec{Environment: "staging"}},
			{Name: "approve-prod", Stage: "approve-prod", Approval: &domain.ApprovalSpec{Required: 1}},
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

// TestSupersededRunServiceGeneration_AtomicWithReviveState pins the read the
// generation-aware cleanup depends on (#97): while a run is superseded it returns the
// service generation being torn down; once a revive clears superseded_by it reports
// NOT superseded, so the cleanup skips rather than deleting the revived pods. Gating
// the generation on superseded_by in one row is what makes that atomic.
func TestSupersededRunServiceGeneration_AtomicWithReviveState(t *testing.T) {
	f := newStage0GateFixture(t, "gensupersede", domain.SupersedeBranch)
	older := f.createRun(t, "main")
	f.createRun(t, "main") // newer supersedes older (canceled + superseded_by set)

	// Give the superseded run a non-zero generation so the read proves it returns the
	// VALUE, not merely presence.
	if _, err := f.pool.Exec(f.ctx, `UPDATE runs SET service_generation = 3 WHERE id = $1`, older.RunID); err != nil {
		t.Fatalf("bump gen: %v", err)
	}

	gen, superseded, err := f.s.SupersededRunServiceGeneration(f.ctx, older.RunID)
	if err != nil {
		t.Fatalf("SupersededRunServiceGeneration: %v", err)
	}
	if !superseded || gen != 3 {
		t.Fatalf("while superseded: got (gen=%d, superseded=%v), want (3, true)", gen, superseded)
	}

	// Simulate a revive clearing superseded_by: the combined read must now report NOT
	// superseded so the cleanup skips (a separate still-superseded + generation read
	// could straddle the revive and delete the revived generation's pods).
	if _, err := f.pool.Exec(f.ctx, `UPDATE runs SET superseded_by = NULL WHERE id = $1`, older.RunID); err != nil {
		t.Fatalf("clear superseded_by: %v", err)
	}
	if gen, superseded, err := f.s.SupersededRunServiceGeneration(f.ctx, older.RunID); err != nil || superseded || gen != 0 {
		t.Fatalf("after revive: got (gen=%d, superseded=%v, err=%v), want (0, false, nil)", gen, superseded, err)
	}
}

// Creating a newer run in a lane clears the older run pending at the same ready
// stage-0 gate, in the SAME create tx — the caller gets the victims on RunCreated.
func TestCreationFire_SupersedesOlderStage0Gate(t *testing.T) {
	f := newStage0GateFixture(t, "firebranch", domain.SupersedeBranch)
	older := f.createRun(t, "main")
	newer := f.createRun(t, "main")

	if len(newer.Superseded) != 1 || newer.Superseded[0].RunID != older.RunID {
		t.Fatalf("newer.Superseded = %+v, want [older #%d]", newer.Superseded, older.Counter)
	}
	if st := f.stateOf(t, older.RunID); st.status != "canceled" || st.supersededBy == nil || *st.supersededBy != newer.RunID {
		t.Fatalf("older run not superseded: %+v", st)
	}
	if st := f.stateOf(t, newer.RunID); st.status != "queued" {
		t.Fatalf("newer run status = %q, want queued", st.status)
	}
	// (run.superseded audit is emitted by the effects listener off the NOTIFY, not
	// here — covered in the scheduler suite.)
}

// The creation fire emits run_superseded (transactional NOTIFY) with the victim's
// run id so the scheduler's effects listener can push CancelJob frames post-commit.
func TestCreationFire_EmitsSupersededNotify(t *testing.T) {
	f := newStage0GateFixture(t, "firenotify", domain.SupersedeBranch)
	older := f.createRun(t, "main")

	conn, err := pgx.Connect(f.ctx, dbtest.DSN())
	if err != nil {
		t.Fatalf("listen conn: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(f.ctx, "LISTEN "+SupersededRunChannel); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	newer := f.createRun(t, "main") // fires supersede on older → pg_notify on commit
	if len(newer.Superseded) != 1 {
		t.Fatalf("expected 1 victim, got %d", len(newer.Superseded))
	}

	waitCtx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	note, err := conn.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("no run_superseded notification: %v", err)
	}
	if note.Channel != SupersededRunChannel || note.Payload != older.RunID.String() {
		t.Fatalf("notify = {chan:%s payload:%s}, want {chan:%s payload:%s}",
			note.Channel, note.Payload, SupersededRunChannel, older.RunID)
	}
}

// The PRIMARY case: in a build→approve→deploy pipeline the gate sits at stage 1, so
// the creation fire doesn't cover it. Completing a run's build stage makes its
// approve-staging gate reachable and fires the cascade supersede, clearing the older
// lane sibling pending at that gate.
func TestCascadeFire_SupersedesOnStageComplete(t *testing.T) {
	f := newGateFixture(t, "cascade") // build → gate-staging → deploy-staging → gate-prod → deploy-prod
	older := f.createRun(t, "main")
	newer := f.createRun(t, "main")

	// Drive newer's single build job (compile) to success via the real CompleteJob
	// path so the cascade + supersedeAfterCascade run exactly as in production.
	var jobID uuid.UUID
	var attempt int32
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id, attempt FROM job_runs WHERE run_id=$1 AND name='compile'`, newer.RunID).Scan(&jobID, &attempt); err != nil {
		t.Fatalf("compile job: %v", err)
	}
	agentID := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE job_runs SET status='running', agent_id=$2, started_at=NOW() WHERE id=$1`, jobID, agentID); err != nil {
		t.Fatalf("assign compile: %v", err)
	}
	comp, ok, err := f.s.CompleteJob(f.ctx, CompleteJobInput{
		JobRunID: jobID, Status: "success", ExpectedAgentID: agentID, ExpectedAttempt: attempt,
	})
	if err != nil || !ok {
		t.Fatalf("CompleteJob: ok=%v err=%v", ok, err)
	}
	if !comp.StageCompleted {
		t.Fatalf("build stage (single job) should have completed")
	}

	if st := f.stateOf(t, older.RunID); st.status != "canceled" || st.supersededBy == nil || *st.supersededBy != newer.RunID {
		t.Fatalf("older run not superseded by the cascade fire: %+v", st)
	}
}

// ClaimSupersedeEffects is exactly-one-claimer with a lease: a live claim blocks a
// second, a claim past the lease is reclaimable (crashed claimer), and once
// MarkSupersedeEffectsDone lands no further claim succeeds (#97 pt.5d).
func TestClaimSupersedeEffects_LeaseReclaim(t *testing.T) {
	f := newGateFixture(t, "claimlease")
	victim := f.createRun(t, "main")
	other := f.createRun(t, "main")
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE runs SET status='canceled', finished_at=NOW(), superseded_by=$2 WHERE id=$1`,
		victim.RunID, other.RunID); err != nil {
		t.Fatalf("mark superseded: %v", err)
	}

	claim := func() (bool, bool) {
		t.Helper()
		ok, first, err := f.s.ClaimSupersedeEffects(f.ctx, victim.RunID)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return ok, first
	}

	if ok, first := claim(); !ok || !first {
		t.Fatalf("first claim should succeed AND be first (ok=%v first=%v)", ok, first)
	}
	if ok, _ := claim(); ok {
		t.Fatal("second claim within the lease must fail (a live claim holds it)")
	}
	// Backdate the claim beyond the lease → the prior claimer looks crashed.
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE runs SET supersede_effects_claimed_at = NOW() - INTERVAL '10 minutes' WHERE id=$1`, victim.RunID); err != nil {
		t.Fatalf("backdate claim: %v", err)
	}
	// A lease-expiry RECLAIM succeeds but is NOT the first claim — so the metric
	// isn't re-counted for the same supersede event.
	if ok, first := claim(); !ok || first {
		t.Fatalf("reclaim past lease should succeed but NOT be first (ok=%v first=%v)", ok, first)
	}
	// Effects done → no further claim, even after a lease would expire.
	if err := f.s.MarkSupersedeEffectsDone(f.ctx, victim.RunID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE runs SET supersede_effects_claimed_at = NOW() - INTERVAL '10 minutes' WHERE id=$1`, victim.RunID); err != nil {
		t.Fatalf("backdate claim 2: %v", err)
	}
	if ok, _ := claim(); ok {
		t.Fatal("no claim should succeed once effects are marked done")
	}
}

// EmitRunSupersededAudit is idempotent (partial unique index) so a lease-reclaim
// replay — which re-claims and re-runs the effects — can't duplicate the audit.
func TestEmitRunSupersededAudit_Idempotent(t *testing.T) {
	f := newGateFixture(t, "auditonce")
	victim := f.createRun(t, "main")
	meta := map[string]any{"superseded_counter": int64(1), "by_counter": int64(2), "by_run_id": other()}
	for i := 0; i < 2; i++ {
		if err := f.s.EmitRunSupersededAudit(f.ctx, victim.RunID, meta); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM audit_events WHERE target_id=$1 AND action='run.superseded'`,
		victim.RunID.String()).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("run.superseded audit rows = %d, want 1 (idempotent)", n)
	}
}

func other() string { return uuid.New().String() }

// supersede: off is a no-op at creation — the older run stays pending.
func TestCreationFire_OffIsNoop(t *testing.T) {
	f := newStage0GateFixture(t, "fireoff", domain.SupersedeOff)
	older := f.createRun(t, "main")
	newer := f.createRun(t, "main")

	if len(newer.Superseded) != 0 {
		t.Fatalf("supersede off still canceled %d victims", len(newer.Superseded))
	}
	if st := f.stateOf(t, older.RunID); st.status != "queued" || st.supersededBy != nil {
		t.Fatalf("older run touched with supersede off: %+v", st)
	}
}

// Different branch = different lane: a run on feature-x must not supersede one on main.
func TestCreationFire_DifferentLaneUntouched(t *testing.T) {
	f := newStage0GateFixture(t, "firelane", domain.SupersedeBranch)
	onMain := f.createRun(t, "main")
	onFeature := f.createRun(t, "feature-x")

	if len(onFeature.Superseded) != 0 {
		t.Fatalf("feature-x run superseded %d main runs — lanes not isolated", len(onFeature.Superseded))
	}
	if st := f.stateOf(t, onMain.RunID); st.status != "queued" || st.supersededBy != nil {
		t.Fatalf("main run canceled by a different-branch run: %+v", st)
	}
}

// A newer run whose ready stage-0 gate governs STAGING must not cancel an older run
// that already passed staging and is pending only PROD.
func TestCreationFire_StagingDoesNotCancelProdPending(t *testing.T) {
	f := newStage0GateFixture(t, "fireenv", domain.SupersedeBranch)
	older := f.createRun(t, "main")
	f.approveGate(t, older.RunID, "approve-staging") // advance older past staging → pending only prod

	newer := f.createRun(t, "main")
	if len(newer.Superseded) != 0 {
		t.Fatalf("staging-ready newer canceled a prod-pending older: %+v", newer.Superseded)
	}
	if st := f.stateOf(t, older.RunID); st.status != "queued" || st.supersededBy != nil {
		t.Fatalf("prod-pending older was superseded by a staging run: %+v", st)
	}
}
