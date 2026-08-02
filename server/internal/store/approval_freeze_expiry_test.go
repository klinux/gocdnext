package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// expiryActor is a canonical freeze/unfreeze actor for the #208 tests.
func expiryActor() FreezeActor {
	return FreezeActor{
		ID:    uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Email: "ops@acme.io",
		Name:  "Ops",
	}
}

// projectOf resolves the fixture pipeline's project (the expiry tx does the same
// JOIN in-tx; here we need it to drive freeze/unfreeze + read the epoch table).
func projectOf(t *testing.T, f gateFixture) uuid.UUID {
	t.Helper()
	var pid uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT project_id FROM pipelines WHERE id = $1`, f.pipelineID).Scan(&pid); err != nil {
		t.Fatalf("project id: %v", err)
	}
	return pid
}

func cancelOriginOf(t *testing.T, f gateFixture, jobRunID uuid.UUID) *string {
	t.Helper()
	var origin *string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT cancel_origin FROM job_runs WHERE id = $1`, jobRunID).Scan(&origin); err != nil {
		t.Fatalf("read cancel_origin: %v", err)
	}
	return origin
}

// ResolveApprovalWindow surfaces the gate's GovernedFreezeEnvs (#208) so the
// expirer can check a freeze on them — computed from the same immutable snapshot
// as the window.
func TestResolveApprovalWindow_ReturnsGovernedFreezeEnvs(t *testing.T) {
	f := newGateFixture(t, "expiry-govenvs")
	run := f.createRun(t, "main")

	_, envs, ok, err := f.s.ResolveApprovalWindow(f.ctx, run.RunID, "approve-staging", 168*time.Hour)
	if err != nil || !ok {
		t.Fatalf("resolve = (ok %v, err %v), want a window", ok, err)
	}
	// approve-staging gates a downstream deploy to `staging`; the exact set may
	// also include later envs, so assert membership, not equality.
	found := false
	for _, e := range envs {
		if e == "staging" {
			found = true
		}
	}
	if !found {
		t.Fatalf("governed freeze envs = %v, want to include \"staging\"", envs)
	}
}

// The headline #208 behaviour: a governed env under a freeze PAUSES the expiry.
// Cancelling a gate the freeze is deliberately holding is the exact bug.
func TestExpireApprovalGate_FrozenEnvPauses(t *testing.T) {
	f := newGateFixture(t, "expiry-frozen")
	run := f.createRun(t, "main")
	gate := jobRunID(t, f, run.RunID, "approve-staging")
	backdateGate(t, f, run.RunID, "approve-staging", 8*24*time.Hour)

	proj := projectOf(t, f)
	if froze, err := f.s.FreezeEnvironment(f.ctx, proj, "staging", expiryActor(), "month-end close"); err != nil || !froze {
		t.Fatalf("freeze staging = (%v, %v), want (true, nil)", froze, err)
	}

	in := f.expireInput(t, gate, run.RunID, 168*time.Hour, []string{"staging"}, "approval timeout (168h)")
	if _, err := f.s.ExpireApprovalGate(f.ctx, in); !errors.Is(err, ErrApprovalGateFrozen) {
		t.Fatalf("expire under freeze err = %v, want ErrApprovalGateFrozen", err)
	}

	// Run untouched, gate still parked — nothing was cancelled.
	if st := f.stateOf(t, run.RunID); st.status == "canceled" {
		t.Fatalf("run was canceled despite the governing env being frozen")
	}
	if g := gateStateOf(t, f, gate); g.status != "awaiting_approval" || g.decision != nil {
		t.Fatalf("gate = %+v, want still awaiting_approval with no decision", g)
	}
}

// The mirror: an un-frozen governed env expires normally, and every cancelled
// job carries cancel_origin='approval_expiry' (the #207-reserved value) so an
// upstream rerun can revive it (a system cancel, not a deliberate user_job one).
func TestExpireApprovalGate_UnfrozenExpiresWithApprovalOrigin(t *testing.T) {
	f := newGateFixture(t, "expiry-unfrozen")
	run := f.createRun(t, "main")
	gate := jobRunID(t, f, run.RunID, "approve-staging")
	backdateGate(t, f, run.RunID, "approve-staging", 8*24*time.Hour)

	// staging is NOT frozen — the gate governs it but the pause doesn't apply.
	in := f.expireInput(t, gate, run.RunID, 168*time.Hour, []string{"staging"}, "approval timeout (168h)")
	if _, err := f.s.ExpireApprovalGate(f.ctx, in); err != nil {
		t.Fatalf("expire un-frozen: %v", err)
	}

	if st := f.stateOf(t, run.RunID); st.status != "canceled" {
		t.Fatalf("run status = %q, want canceled", st.status)
	}
	if o := cancelOriginOf(t, f, gate); o == nil || *o != "approval_expiry" {
		t.Fatalf("gate cancel_origin = %v, want approval_expiry", o)
	}
	// A sibling queued job carries the same origin (CancelQueuedJobsInRun).
	compile := jobRunID(t, f, run.RunID, "compile")
	if o := cancelOriginOf(t, f, compile); o == nil || *o != "approval_expiry" {
		t.Fatalf("compile cancel_origin = %v, want approval_expiry", o)
	}
}

// Lifting a freeze grants a FRESH window via the last_unfrozen_at floor: a gate
// whose awaiting_since is long past still does NOT expire right after an
// unfreeze, because effective_start jumps to the unfreeze instant.
func TestExpireApprovalGate_UnfreezeGrantsFreshWindow(t *testing.T) {
	f := newGateFixture(t, "expiry-floor")
	run := f.createRun(t, "main")
	gate := jobRunID(t, f, run.RunID, "approve-staging")
	backdateGate(t, f, run.RunID, "approve-staging", 30*24*time.Hour) // 30 days parked

	proj := projectOf(t, f)
	if froze, err := f.s.FreezeEnvironment(f.ctx, proj, "staging", expiryActor(), "freeze"); err != nil || !froze {
		t.Fatalf("freeze: (%v, %v)", froze, err)
	}
	if removed, err := f.s.UnfreezeEnvironment(f.ctx, proj, "staging", expiryActor()); err != nil || !removed {
		t.Fatalf("unfreeze: (%v, %v), want (true, nil)", removed, err)
	}

	// awaiting_since is 30 days old and the window is 168h, so WITHOUT the floor
	// this would expire. The just-set floor pushes effective_start to ~now.
	in := f.expireInput(t, gate, run.RunID, 168*time.Hour, []string{"staging"}, "approval timeout (168h)")
	if _, err := f.s.ExpireApprovalGate(f.ctx, in); !errors.Is(err, ErrApprovalGateWithinWindow) {
		t.Fatalf("expire right after unfreeze err = %v, want ErrApprovalGateWithinWindow", err)
	}
	if st := f.stateOf(t, run.RunID); st.status == "canceled" {
		t.Fatalf("run canceled despite a fresh post-unfreeze window")
	}
}

// TOCTOU: a rerun re-parks the gate (re-stamping awaiting_since) between the
// candidate scan and the expiry write. The (run_id, awaiting_since) guard makes
// the stale expiry a no-op instead of cancelling a freshly re-armed gate.
func TestExpireApprovalGate_ReparkedGateRefused(t *testing.T) {
	f := newGateFixture(t, "expiry-repark")
	run := f.createRun(t, "main")
	gate := jobRunID(t, f, run.RunID, "approve-staging")
	backdateGate(t, f, run.RunID, "approve-staging", 8*24*time.Hour)

	// Capture at scan time (awaiting_since = 8 days ago).
	in := f.expireInput(t, gate, run.RunID, 168*time.Hour, nil, "approval timeout (168h)")

	// A rerun re-parks the gate to a DIFFERENT awaiting_since.
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE job_runs SET awaiting_since = NOW() - interval '3 days' WHERE id = $1`, gate); err != nil {
		t.Fatalf("re-park: %v", err)
	}

	if _, err := f.s.ExpireApprovalGate(f.ctx, in); !errors.Is(err, ErrApprovalGateDecided) {
		t.Fatalf("expire of a re-parked gate err = %v, want ErrApprovalGateDecided", err)
	}
	if st := f.stateOf(t, run.RunID); st.status == "canceled" {
		t.Fatalf("run canceled despite the gate being re-parked under us")
	}
	if g := gateStateOf(t, f, gate); g.status != "awaiting_approval" {
		t.Fatalf("gate status = %q, want awaiting_approval (untouched)", g.status)
	}
}

// A concurrent freeze/unfreeze (or deploy admission) holding the per-(project,
// env) freeze advisory lock makes the expiry's NON-BLOCKING try-lock back off:
// benign skip, not a wait or an error the caller must special-case.
func TestExpireApprovalGate_LockContentionSkips(t *testing.T) {
	f := newGateFixture(t, "expiry-contended")
	run := f.createRun(t, "main")
	gate := jobRunID(t, f, run.RunID, "approve-staging")
	backdateGate(t, f, run.RunID, "approve-staging", 8*24*time.Hour)
	proj := projectOf(t, f)

	// Hold the freeze advisory lock on a dedicated connection for the duration.
	conn, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if _, err := tx.Exec(f.ctx, `SELECT pg_advisory_xact_lock($1)`, ProjectEnvFreezeLockKey(proj, "staging")); err != nil {
		t.Fatalf("hold lock: %v", err)
	}

	in := f.expireInput(t, gate, run.RunID, 168*time.Hour, []string{"staging"}, "approval timeout (168h)")
	if _, err := f.s.ExpireApprovalGate(f.ctx, in); !errors.Is(err, ErrApprovalGateContended) {
		t.Fatalf("expire under lock contention err = %v, want ErrApprovalGateContended", err)
	}
	if st := f.stateOf(t, run.RunID); st.status == "canceled" {
		t.Fatalf("run canceled despite the freeze lock being contended")
	}
}

// A native (server-managed, agent_id NULL) job running when a gate expires must
// get the durable cancel stamp + origin but must NOT enter the CancelJob fanout —
// framing it would mean Dispatch(uuid.Nil). Since #207 dropped the agent_id filter
// from StampCancelRequestedAtForRun, the RETURNING set now includes native jobs,
// so the expiry has to split them out. An agent-owned running job still appears.
func TestExpireApprovalGate_NativeRunningJobNotInFanout(t *testing.T) {
	f := newGateFixture(t, "expiry-native")
	run := f.createRun(t, "main")
	gate := jobRunID(t, f, run.RunID, "approve-staging")
	backdateGate(t, f, run.RunID, "approve-staging", 8*24*time.Hour)

	// An agent-owned build still running (stage 0) — belongs in the fanout.
	var agentID uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO agents (name, token_hash) VALUES ($1, 'h') RETURNING id`,
		"native-agent-"+run.RunID.String()[:8]).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	compile := jobRunID(t, f, run.RunID, "compile")
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW() WHERE id=$2`, agentID, compile); err != nil {
		t.Fatalf("flip compile running: %v", err)
	}
	// A native deploy running with NO agent — must be stamped but not framed.
	native := jobRunID(t, f, run.RunID, "dep-staging")
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE job_runs SET status='running', agent_id=NULL, started_at=NOW() WHERE id=$1`, native); err != nil {
		t.Fatalf("flip native running: %v", err)
	}

	in := f.expireInput(t, gate, run.RunID, 168*time.Hour, nil, "approval timeout (168h)")
	res, err := f.s.ExpireApprovalGate(f.ctx, in)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}

	// The native job is stamped (durable) with the approval_expiry origin...
	var canceledAt *time.Time
	if o := cancelOriginOf(t, f, native); o == nil || *o != "approval_expiry" {
		t.Fatalf("native cancel_origin = %v, want approval_expiry", o)
	}
	if err := f.pool.QueryRow(f.ctx,
		`SELECT cancel_requested_at FROM job_runs WHERE id=$1`, native).Scan(&canceledAt); err != nil {
		t.Fatalf("read native cancel_requested_at: %v", err)
	}
	if canceledAt == nil {
		t.Fatalf("native job missing cancel_requested_at stamp")
	}

	// ...but never appears in the fanout, and no ref carries a nil agent.
	sawCompile, sawNative := false, false
	for _, r := range res.RunningJobs {
		if r.AgentID == uuid.Nil {
			t.Fatalf("RunningJobs carries a nil-agent ref → Dispatch(uuid.Nil): %+v", r)
		}
		switch r.JobID {
		case compile:
			sawCompile = true
		case native:
			sawNative = true
		}
	}
	if !sawCompile {
		t.Fatalf("agent-owned compile missing from the fanout: %+v", res.RunningJobs)
	}
	if sawNative {
		t.Fatalf("native job must NOT be in the fanout: %+v", res.RunningJobs)
	}
}

// The floor is stamped ONLY when an unfreeze actually removed a freeze: an
// idempotent unfreeze (nothing frozen) must not renew a window nobody waits on.
func TestUnfreezeEnvironment_StampsEpochOnlyOnRealRemoval(t *testing.T) {
	f := newGateFixture(t, "unfreeze-epoch")
	proj := projectOf(t, f)

	epochCount := func() int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(f.ctx,
			`SELECT COUNT(*) FROM environment_freeze_epochs WHERE project_id = $1 AND environment = 'staging'`,
			proj).Scan(&n); err != nil {
			t.Fatalf("epoch count: %v", err)
		}
		return n
	}

	// Nothing frozen → unfreeze is a no-op → NO epoch row.
	if removed, err := f.s.UnfreezeEnvironment(f.ctx, proj, "staging", expiryActor()); err != nil || removed {
		t.Fatalf("idempotent unfreeze = (%v, %v), want (false, nil)", removed, err)
	}
	if n := epochCount(); n != 0 {
		t.Fatalf("epoch rows after a no-op unfreeze = %d, want 0", n)
	}

	// Freeze then unfreeze → real removal → exactly one epoch row, recent.
	if _, err := f.s.FreezeEnvironment(f.ctx, proj, "staging", expiryActor(), "freeze"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if removed, err := f.s.UnfreezeEnvironment(f.ctx, proj, "staging", expiryActor()); err != nil || !removed {
		t.Fatalf("real unfreeze = (%v, %v), want (true, nil)", removed, err)
	}
	if n := epochCount(); n != 1 {
		t.Fatalf("epoch rows after a real unfreeze = %d, want 1", n)
	}
	var ts time.Time
	if err := f.pool.QueryRow(f.ctx,
		`SELECT last_unfrozen_at FROM environment_freeze_epochs WHERE project_id = $1 AND environment = 'staging'`,
		proj).Scan(&ts); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if time.Since(ts) > time.Minute {
		t.Fatalf("last_unfrozen_at = %v, want ~now", ts)
	}
}
