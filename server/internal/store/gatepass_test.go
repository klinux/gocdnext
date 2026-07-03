package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func TestLaneEnvLockKey(t *testing.T) {
	pid := uuid.New()
	other := uuid.New()
	// branch mode keys on ref; pipeline mode ignores it.
	if LaneEnvLockKey(pid, domain.SupersedeBranch, "main", "prod") ==
		LaneEnvLockKey(pid, domain.SupersedeBranch, "feature", "prod") {
		t.Fatal("branch-mode key must depend on ref")
	}
	if LaneEnvLockKey(pid, domain.SupersedePipeline, "main", "prod") !=
		LaneEnvLockKey(pid, domain.SupersedePipeline, "feature", "prod") {
		t.Fatal("pipeline-mode key must ignore ref")
	}
	// mode-distinct (branch:"" vs pipeline:""), env-distinct, pipeline-distinct, stable.
	if LaneEnvLockKey(pid, domain.SupersedeBranch, "", "prod") ==
		LaneEnvLockKey(pid, domain.SupersedePipeline, "", "prod") {
		t.Fatal("branch and pipeline modes must not collide")
	}
	if LaneEnvLockKey(pid, domain.SupersedeBranch, "main", "prod") ==
		LaneEnvLockKey(pid, domain.SupersedeBranch, "main", "staging") {
		t.Fatal("env must change the key")
	}
	if LaneEnvLockKey(pid, domain.SupersedeBranch, "main", "prod") ==
		LaneEnvLockKey(other, domain.SupersedeBranch, "main", "prod") {
		t.Fatal("pipeline must change the key")
	}
	stable1 := LaneEnvLockKey(pid, domain.SupersedeBranch, "main", "prod")
	stable2 := LaneEnvLockKey(pid, domain.SupersedeBranch, "main", "prod")
	if stable1 != stable2 {
		t.Fatal("key must be stable")
	}
}

// newMarkerFixture applies an arbitrary supersede-configured pipeline (the caller
// supplies Name/Supersede/Stages/Jobs; the git material is attached here).
func newMarkerFixture(t *testing.T, slug string, def domain.Pipeline) gateFixture {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := New(pool)
	ctx := context.Background()
	url := "https://github.com/acme/" + slug
	fp := domain.GitFingerprint(url, "main")
	def.Materials = []domain.Material{{
		Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
		Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
	}}
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

func (f gateFixture) gateJobID(t *testing.T, runID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM job_runs WHERE run_id=$1 AND name=$2`, runID, name).Scan(&id); err != nil {
		t.Fatalf("gate job id %s: %v", name, err)
	}
	return id
}

func (f gateFixture) markerEnvs(t *testing.T, runID uuid.UUID) []string {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT environment FROM run_gate_pass WHERE run_id=$1 ORDER BY environment`, runID)
	if err != nil {
		t.Fatalf("markers: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func deployJob(name, stage, env string, needs ...string) domain.Job {
	return domain.Job{Name: name, Stage: stage, Image: "alpine", Tasks: []domain.Task{{Script: "true"}},
		Deploy: &domain.DeploySpec{Environment: env}, Needs: needs}
}
func gateJob(name, stage string) domain.Job {
	return domain.Job{Name: name, Stage: stage, Approval: &domain.ApprovalSpec{Required: 1}}
}

// Approving the staging gate writes exactly the {staging} marker (prod gate untouched).
func TestGatePass_SingleGateWritesMarker(t *testing.T) {
	f := newGateFixture(t, "gpsingle") // build→gate-staging→deploy-staging→gate-prod→deploy-prod
	run := f.createRun(t, "main")
	if _, err := f.s.ApproveGate(f.ctx, ApprovalDecision{JobRunID: f.gateJobID(t, run.RunID, "approve-staging")}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := f.markerEnvs(t, run.RunID); !reflect.DeepEqual(got, []string{"staging"}) {
		t.Fatalf("markers = %v, want [staging]", got)
	}
}

// A gate governing no deploy (pure-approval pipeline) writes no marker.
func TestGatePass_NoDeployNoMarker(t *testing.T) {
	f := newMarkerFixture(t, "gpnodeploy", domain.Pipeline{
		Name: "p1", Supersede: domain.SupersedeBranch,
		Stages: []string{"approve"},
		Jobs:   []domain.Job{gateJob("gate", "approve")},
	})
	run := f.createRun(t, "main")
	if _, err := f.s.ApproveGate(f.ctx, ApprovalDecision{JobRunID: f.gateJobID(t, run.RunID, "gate")}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := f.markerEnvs(t, run.RunID); len(got) != 0 {
		t.Fatalf("markers = %v, want none (gate governs no deploy)", got)
	}
}

// supersede: off writes no marker (the backstop only guards supersede pipelines).
func TestGatePass_SupersedeOffNoMarker(t *testing.T) {
	f := newMarkerFixture(t, "gpoff", domain.Pipeline{
		Name: "p1", Supersede: domain.SupersedeOff,
		Stages: []string{"approve", "deploy"},
		Jobs:   []domain.Job{gateJob("gate", "approve"), deployJob("dep", "deploy", "prod")},
	})
	run := f.createRun(t, "main")
	if _, err := f.s.ApproveGate(f.ctx, ApprovalDecision{JobRunID: f.gateJobID(t, run.RunID, "gate")}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := f.markerEnvs(t, run.RunID); len(got) != 0 {
		t.Fatalf("markers = %v, want none (supersede off)", got)
	}
}

// Multi-gate env: the marker lands only after ALL governing gates pass.
func TestGatePass_MultiGateMarkerAfterAll(t *testing.T) {
	f := multiGateFixture(t, "gpmulti")
	run := f.createRun(t, "main")

	if _, err := f.s.ApproveGate(f.ctx, ApprovalDecision{JobRunID: f.gateJobID(t, run.RunID, "approve-sec")}); err != nil {
		t.Fatalf("approve sec: %v", err)
	}
	if got := f.markerEnvs(t, run.RunID); len(got) != 0 {
		t.Fatalf("markers after 1/2 gates = %v, want none", got)
	}
	if _, err := f.s.ApproveGate(f.ctx, ApprovalDecision{JobRunID: f.gateJobID(t, run.RunID, "approve-ops")}); err != nil {
		t.Fatalf("approve ops: %v", err)
	}
	if got := f.markerEnvs(t, run.RunID); !reflect.DeepEqual(got, []string{"prod"}) {
		t.Fatalf("markers after 2/2 gates = %v, want [prod]", got)
	}
}

// The review HIGH: two users approving the two gates of one env CONCURRENTLY must
// yield exactly one marker — the per-env advisory lock serializes the "all passed"
// eval, so the second writer sees both passed and the first skips. WITHOUT the lock
// the outcome is racy (both can read the other still-pending and BOTH skip → zero
// markers), so we loop over fresh runs: with the lock every run yields exactly 1;
// without it, at least one run drops to 0 and the test fails.
func TestGatePass_ConcurrentMultiGateExactlyOneMarker(t *testing.T) {
	f := multiGateFixture(t, "gpconc")
	const rounds = 8
	for i := 0; i < rounds; i++ {
		run := f.createRun(t, "main")
		secID := f.gateJobID(t, run.RunID, "approve-sec")
		opsID := f.gateJobID(t, run.RunID, "approve-ops")

		var wg sync.WaitGroup
		errCh := make(chan error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); _, err := f.s.ApproveGate(f.ctx, ApprovalDecision{JobRunID: secID}); errCh <- err }()
		go func() { defer wg.Done(); _, err := f.s.ApproveGate(f.ctx, ApprovalDecision{JobRunID: opsID}); errCh <- err }()
		wg.Wait()
		close(errCh)
		for err := range errCh {
			if err != nil {
				t.Fatalf("round %d: concurrent approve errored: %v", i, err)
			}
		}

		var n int
		if err := f.pool.QueryRow(f.ctx,
			`SELECT count(*) FROM run_gate_pass WHERE run_id=$1 AND environment='prod'`, run.RunID).Scan(&n); err != nil {
			t.Fatalf("round %d: count markers: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("round %d: prod markers = %d, want exactly 1 (advisory lock must serialize)", i, n)
		}
	}
}

// The MED: the marker must resolve gate->env from the RUN's definition snapshot,
// not the live pipelines.definition (upserted in place by ApplyProject). Create a
// run whose gate governs prod, DRIFT the live definition so the gate would govern
// staging, then approve — the marker must still be prod (the run's shape).
func TestGatePass_UsesRunSnapshotNotLiveDefinition(t *testing.T) {
	def := domain.Pipeline{
		Name: "p1", Supersede: domain.SupersedeBranch,
		Stages: []string{"build", "approve", "deploy"},
		Jobs: []domain.Job{
			{Name: "compile", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}},
			gateJob("gate", "approve"),
			deployJob("dep", "deploy", "prod"),
		},
	}
	f := newMarkerFixture(t, "gpdrift", def)
	run := f.createRun(t, "main") // snapshots def: gate governs prod

	// Drift the LIVE pipeline definition: the gate now governs staging.
	mutated := def
	mutated.Jobs = append([]domain.Job(nil), def.Jobs...)
	mutated.Jobs[2] = deployJob("dep", "deploy", "staging")
	raw, err := json.Marshal(mutated)
	if err != nil {
		t.Fatalf("marshal mutated def: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE pipelines SET definition=$2 WHERE id=$1`, f.pipelineID, raw); err != nil {
		t.Fatalf("drift pipeline def: %v", err)
	}

	if _, err := f.s.ApproveGate(f.ctx, ApprovalDecision{JobRunID: f.gateJobID(t, run.RunID, "gate")}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := f.markerEnvs(t, run.RunID); !reflect.DeepEqual(got, []string{"prod"}) {
		t.Fatalf("marker = %v, want [prod] from the run snapshot (live def drifted to staging)", got)
	}
}

// Approving a gate whose RUN was superseded returns ErrApprovalSuperseded (distinct
// from "already decided") and records NO orphan vote — the gate row lock + the
// superseded check land before the vote insert.
func TestApprove_SupersededRunErrorsNoOrphanVote(t *testing.T) {
	f := newGateFixture(t, "apprsup")
	v := f.createRun(t, "main")
	newer := f.createRun(t, "main")
	gate := f.gateJobID(t, v.RunID, "approve-staging")

	// Supersede v exactly as the terminalizer does: run canceled+superseded_by, gates
	// canceled.
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE runs SET status='canceled', finished_at=NOW(), superseded_by=$2, cancel_reason='superseded by #2' WHERE id=$1`,
		v.RunID, newer.RunID); err != nil {
		t.Fatalf("supersede run: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE job_runs SET status='canceled' WHERE run_id=$1`, v.RunID); err != nil {
		t.Fatalf("cancel jobs: %v", err)
	}

	_, err := f.s.ApproveGate(f.ctx, ApprovalDecision{JobRunID: gate, UserID: uuid.New(), User: "alice"})
	if !errors.Is(err, ErrApprovalSuperseded) {
		t.Fatalf("approve superseded gate = %v, want ErrApprovalSuperseded", err)
	}
	var votes int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM job_run_approvals WHERE job_run_id=$1`, gate).Scan(&votes); err != nil {
		t.Fatalf("count votes: %v", err)
	}
	if votes != 0 {
		t.Fatalf("orphan vote recorded on a superseded gate: %d", votes)
	}
}

// Rerunning a job of a superseded run revives it: the supersede state (badge +
// effects-claim) is fully cleared so it's a live run again and a future re-supersede
// can fire its effects.
func TestRerunRevive_ClearsSupersedeState(t *testing.T) {
	f := newGateFixture(t, "revive")
	v := f.createRun(t, "main")
	newer := f.createRun(t, "main")
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE runs SET status='canceled', finished_at=NOW(), superseded_by=$2,
		    cancel_reason='superseded by #2', supersede_effects_at=NOW(), supersede_effects_claimed_at=NOW()
		WHERE id=$1`, v.RunID, newer.RunID); err != nil {
		t.Fatalf("supersede run: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE job_runs SET status='canceled' WHERE run_id=$1`, v.RunID); err != nil {
		t.Fatalf("cancel jobs: %v", err)
	}

	if _, err := f.s.RerunJob(f.ctx, RerunJobInput{JobRunID: f.gateJobID(t, v.RunID, "compile"), TriggeredBy: "user:alice"}); err != nil {
		t.Fatalf("rerun job: %v", err)
	}

	var status string
	var supersededBy *uuid.UUID
	var cancelReason *string
	var effectsAt, claimedAt *time.Time
	if err := f.pool.QueryRow(f.ctx,
		`SELECT status, superseded_by, cancel_reason, supersede_effects_at, supersede_effects_claimed_at FROM runs WHERE id=$1`,
		v.RunID).Scan(&status, &supersededBy, &cancelReason, &effectsAt, &claimedAt); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "running" || supersededBy != nil || cancelReason != nil || effectsAt != nil || claimedAt != nil {
		t.Fatalf("revive left supersede state: status=%q by=%v reason=%v effects_at=%v claimed_at=%v",
			status, supersededBy, cancelReason, effectsAt, claimedAt)
	}
}

// Rerun-revive drops gate-pass markers for RE-ARMED (downstream) gates but keeps
// markers for still-passed (upstream) gates — deleting the latter would drop a
// legitimate block and let an older run's stale deploy through.
func TestRerunRevive_ClearsDownstreamMarkersKeepsUpstream(t *testing.T) {
	f := newGateFixture(t, "revmark")
	v := f.createRun(t, "main")
	newer := f.createRun(t, "main")

	// v cleared staging (upstream gate success + marker), was superseded before prod.
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE job_runs SET status='success' WHERE run_id=$1 AND name IN ('compile','approve-staging')`, v.RunID); err != nil {
		t.Fatalf("pass upstream: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE job_runs SET status='canceled' WHERE run_id=$1 AND name IN ('dep-staging','approve-prod','dep-prod')`, v.RunID); err != nil {
		t.Fatalf("cancel downstream: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO run_gate_pass (run_id, pipeline_id, ref, counter, environment)
		VALUES ($1,$2,'main',$3,'staging'), ($1,$2,'main',$3,'prod')`,
		v.RunID, f.pipelineID, v.Counter); err != nil {
		t.Fatalf("seed markers: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE runs SET status='canceled', finished_at=NOW(), superseded_by=$2 WHERE id=$1`, v.RunID, newer.RunID); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	// Rerun the staging deploy (stage 2) → re-arms approve-prod (stage 3), not
	// approve-staging (stage 1).
	if _, err := f.s.RerunJob(f.ctx, RerunJobInput{JobRunID: f.gateJobID(t, v.RunID, "dep-staging"), TriggeredBy: "user:alice"}); err != nil {
		t.Fatalf("rerun job: %v", err)
	}
	if got := f.markerEnvs(t, v.RunID); !reflect.DeepEqual(got, []string{"staging"}) {
		t.Fatalf("markers after revive = %v, want [staging] (prod re-armed→cleared, staging kept)", got)
	}
}

// HIGH #1 layer 1: an approval gate can never be rerun (it would re-queue and then
// dispatch as a task-less job, bypassing approval).
func TestRerunJob_RefusesApprovalGate(t *testing.T) {
	f := newGateFixture(t, "rerungate")
	v := f.createRun(t, "main")
	_, err := f.s.RerunJob(f.ctx, RerunJobInput{JobRunID: f.gateJobID(t, v.RunID, "approve-staging"), TriggeredBy: "u"})
	if !errors.Is(err, ErrCannotRerunGate) {
		t.Fatalf("rerun of a gate = %v, want ErrCannotRerunGate", err)
	}
}

// HIGH #1 layer 2: even a gate somehow left 'queued' is never returned by the
// dispatch query.
func TestListDispatchableJobs_ExcludesApprovalGate(t *testing.T) {
	f := newGateFixture(t, "dispgate")
	v := f.createRun(t, "main")
	gate := f.gateJobID(t, v.RunID, "approve-staging")
	// Make the gate's stage the active one and force the gate to 'queued' (the bug shape).
	if _, err := f.pool.Exec(f.ctx, `UPDATE job_runs SET status='success' WHERE run_id=$1 AND name='compile'`, v.RunID); err != nil {
		t.Fatalf("finish build: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE stage_runs SET status='success' WHERE run_id=$1 AND name='build'`, v.RunID); err != nil {
		t.Fatalf("finish build stage: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE job_runs SET status='queued' WHERE id=$1`, gate); err != nil {
		t.Fatalf("force gate queued: %v", err)
	}
	jobs, err := f.s.ListDispatchableJobs(f.ctx, v.RunID)
	if err != nil {
		t.Fatalf("list dispatchable: %v", err)
	}
	for _, j := range jobs {
		if j.ID == gate {
			t.Fatalf("dispatch query returned an approval gate (%s) — would run as a task-less job", gate)
		}
	}
}

// HIGH #2: re-arming a gate on rerun drops the stale votes it held, so an old
// pre-cancel vote can't count toward the fresh quorum.
func TestRerunRevive_ClearsReArmedGateVotes(t *testing.T) {
	f := newGateFixture(t, "revvotes")
	v := f.createRun(t, "main")
	newer := f.createRun(t, "main")
	prodGate := f.gateJobID(t, v.RunID, "approve-prod")

	var userID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO users (provider, external_id, email) VALUES ('test','ext-1','a@acme.test') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO job_run_approvals (job_run_id, user_id, user_label, decision) VALUES ($1,$2,'alice','approved')`,
		prodGate, userID); err != nil {
		t.Fatalf("seed vote: %v", err)
	}

	// Supersede v with approve-staging passed; rerun dep-staging (stage 2) re-arms
	// approve-prod (stage 3).
	if _, err := f.pool.Exec(f.ctx, `UPDATE job_runs SET status='success' WHERE run_id=$1 AND name IN ('compile','approve-staging')`, v.RunID); err != nil {
		t.Fatalf("pass upstream: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE job_runs SET status='canceled' WHERE run_id=$1 AND name IN ('dep-staging','approve-prod','dep-prod')`, v.RunID); err != nil {
		t.Fatalf("cancel downstream: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE runs SET status='canceled', finished_at=NOW(), superseded_by=$2 WHERE id=$1`, v.RunID, newer.RunID); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	if _, err := f.s.RerunJob(f.ctx, RerunJobInput{JobRunID: f.gateJobID(t, v.RunID, "dep-staging"), TriggeredBy: "u"}); err != nil {
		t.Fatalf("rerun: %v", err)
	}

	var votes int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM job_run_approvals WHERE job_run_id=$1`, prodGate).Scan(&votes); err != nil {
		t.Fatalf("count votes: %v", err)
	}
	if votes != 0 {
		t.Fatalf("re-armed gate kept %d stale votes (quorum bypass)", votes)
	}
}

// MED primitive: RunStillSuperseded distinguishes a superseded run from a revived one
// (used by the effects worker to skip a cleanup that would tear down a revived run).
func TestRunStillSuperseded(t *testing.T) {
	f := newGateFixture(t, "stillsup")
	v := f.createRun(t, "main")
	if yes, err := f.s.RunStillSuperseded(f.ctx, v.RunID); err != nil || yes {
		t.Fatalf("fresh run: superseded=%v err=%v, want false", yes, err)
	}
	newer := f.createRun(t, "main")
	if _, err := f.pool.Exec(f.ctx, `UPDATE runs SET superseded_by=$2 WHERE id=$1`, v.RunID, newer.RunID); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if yes, err := f.s.RunStillSuperseded(f.ctx, v.RunID); err != nil || !yes {
		t.Fatalf("superseded run: superseded=%v err=%v, want true", yes, err)
	}
}

func multiGateFixture(t *testing.T, slug string) gateFixture {
	return newMarkerFixture(t, slug, domain.Pipeline{
		Name: "p1", Supersede: domain.SupersedeBranch,
		Stages: []string{"build", "approve", "deploy"},
		Jobs: []domain.Job{
			{Name: "compile", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}},
			gateJob("approve-sec", "approve"),
			gateJob("approve-ops", "approve"),
			deployJob("deploy-prod", "deploy", "prod", "approve-sec", "approve-ops"),
		},
	})
}
