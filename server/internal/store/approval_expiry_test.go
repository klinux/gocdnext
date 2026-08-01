package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// backdateGate rewinds a gate's awaiting_since so a test can reach an expiry
// window without sleeping through it.
func backdateGate(t *testing.T, f gateFixture, runID uuid.UUID, gate string, age time.Duration) {
	t.Helper()
	ct, err := f.pool.Exec(f.ctx,
		`UPDATE job_runs SET awaiting_since = NOW() - $3::interval
		 WHERE run_id = $1 AND name = $2 AND status = 'awaiting_approval'`,
		runID, gate, age.String())
	if err != nil {
		t.Fatalf("backdate gate: %v", err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("backdate gate %q: expected 1 row, got %d", gate, ct.RowsAffected())
	}
}

func jobRunID(t *testing.T, f gateFixture, runID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`SELECT id FROM job_runs WHERE run_id = $1 AND name = $2`, runID, name).Scan(&id); err != nil {
		t.Fatalf("job run id for %q: %v", name, err)
	}
	return id
}

type gateState struct {
	status    string
	decision  *string
	decidedBy *string
}

func gateStateOf(t *testing.T, f gateFixture, jobRunID uuid.UUID) gateState {
	t.Helper()
	var g gateState
	if err := f.pool.QueryRow(f.ctx,
		`SELECT status, decision, decided_by FROM job_runs WHERE id = $1`, jobRunID,
	).Scan(&g.status, &g.decision, &g.decidedBy); err != nil {
		t.Fatalf("read gate state: %v", err)
	}
	return g
}

func TestListPendingApprovalGates(t *testing.T) {
	f := newGateFixture(t, "expiry-list")
	old := f.createRun(t, "main")
	fresh := f.createRun(t, "main")

	backdateGate(t, f, old.RunID, "approve-staging", 3*time.Hour)

	// Cutoff at one minute: the backdated gate is well past it, the fresh run's
	// gates were stamped NOW() and must not surface.
	got, err := f.s.ListPendingApprovalGates(f.ctx, time.Now().Add(-time.Minute), ApprovalGateCursor{}, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var sawOld, sawFresh bool
	for _, c := range got {
		switch c.RunID {
		case old.RunID:
			if c.JobName == "approve-staging" {
				sawOld = true
			}
		case fresh.RunID:
			sawFresh = true
		}
	}
	if !sawOld {
		t.Fatalf("backdated gate missing from candidates: %+v", got)
	}
	if sawFresh {
		t.Fatalf("gate inside its window must not be a candidate: %+v", got)
	}
}

// A run that already reached a terminal status must never surface: cancelling
// it again would be a no-op at best and a confusing audit trail at worst.
func TestListPendingApprovalGates_SkipsTerminalRuns(t *testing.T) {
	f := newGateFixture(t, "expiry-terminal")
	run := f.createRun(t, "main")
	backdateGate(t, f, run.RunID, "approve-staging", 3*time.Hour)

	if _, err := f.pool.Exec(f.ctx, `UPDATE runs SET status='success' WHERE id=$1`, run.RunID); err != nil {
		t.Fatalf("terminalize: %v", err)
	}
	got, err := f.s.ListPendingApprovalGates(f.ctx, time.Now().Add(-time.Minute), ApprovalGateCursor{}, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, c := range got {
		if c.RunID == run.RunID {
			t.Fatalf("terminal run surfaced as a candidate: %+v", c)
		}
	}
}

func TestResolveApprovalWindow(t *testing.T) {
	f := newGateFixture(t, "expiry-window")
	run := f.createRun(t, "main")

	// The fixture's gates declare no timeout, so they inherit whatever the
	// server default is.
	d, ok, err := f.s.ResolveApprovalWindow(f.ctx, run.RunID, "approve-staging", 168*time.Hour)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !ok || d != 168*time.Hour {
		t.Fatalf("inherited window = (%v, %v), want (168h, true)", d, ok)
	}

	// No server default and no gate window: nothing expires.
	if _, ok, err := f.s.ResolveApprovalWindow(f.ctx, run.RunID, "approve-staging", 0); err != nil || ok {
		t.Fatalf("with no default, expected (_, false, nil), got (_, %v, %v)", ok, err)
	}

	// A job that isn't a gate in the definition resolves to "never expire" —
	// fail-safe, since cancelling a run we can't explain is the worse error.
	if _, ok, err := f.s.ResolveApprovalWindow(f.ctx, run.RunID, "compile", 168*time.Hour); err != nil || ok {
		t.Fatalf("non-gate job should not expire, got (%v, %v)", ok, err)
	}
	if _, ok, err := f.s.ResolveApprovalWindow(f.ctx, run.RunID, "ghost", 168*time.Hour); err != nil || ok {
		t.Fatalf("unknown job should not expire, got (%v, %v)", ok, err)
	}
}

// The headline behaviour: an abandoned gate terminalises its run as CANCELED,
// never FAILED. The dashboard computes success rate as
// success/(success+failed) with canceled excluded, so getting this wrong would
// silently degrade every pipeline's success metric — the exact reason the
// documented `failed` behaviour was rejected.
func TestExpireApprovalGate_CancelsNeverFails(t *testing.T) {
	f := newGateFixture(t, "expiry-cancel")
	run := f.createRun(t, "main")
	gate := jobRunID(t, f, run.RunID, "approve-staging")
	backdateGate(t, f, run.RunID, "approve-staging", 8*24*time.Hour)

	if _, err := f.s.ExpireApprovalGate(f.ctx, gate, run.RunID, "approval timeout (168h)"); err != nil {
		t.Fatalf("expire: %v", err)
	}

	st := f.stateOf(t, run.RunID)
	if st.status != "canceled" {
		t.Fatalf("run status = %q, want canceled (failed would poison the success rate)", st.status)
	}
	if st.cancelReason == nil || *st.cancelReason != "approval timeout (168h)" {
		t.Fatalf("cancel_reason = %v, want the timeout reason", st.cancelReason)
	}
	// superseded_by must stay NULL — this was not a supersede, and the UI
	// renders a different badge for each.
	if st.supersededBy != nil {
		t.Fatalf("superseded_by = %v, want nil", st.supersededBy)
	}

	g := gateStateOf(t, f, gate)
	if g.status != "canceled" {
		t.Fatalf("gate status = %q, want canceled — a leftover awaiting_approval row is a ghost in the UI", g.status)
	}
	if g.decision == nil || *g.decision != "expired" {
		t.Fatalf("gate decision = %v, want \"expired\" (distinct from reject)", g.decision)
	}
	if g.decidedBy != nil {
		t.Fatalf("decided_by = %v, want nil — nobody decided", g.decidedBy)
	}

	// The whole run drains: the second gate and every queued job go with it,
	// otherwise the canceled run keeps a "ready to approve" ghost.
	other := gateStateOf(t, f, jobRunID(t, f, run.RunID, "approve-prod"))
	if other.status != "canceled" {
		t.Fatalf("downstream gate status = %q, want canceled", other.status)
	}
}

// A human clicking between the candidate scan and the expiry write must win.
// This is the race the whole feature has to lose gracefully: the decision the
// expirer existed to force already happened.
func TestExpireApprovalGate_DecidedUnderUsIsRefused(t *testing.T) {
	f := newGateFixture(t, "expiry-race")
	run := f.createRun(t, "main")
	gate := jobRunID(t, f, run.RunID, "approve-staging")
	backdateGate(t, f, run.RunID, "approve-staging", 8*24*time.Hour)

	f.approveGate(t, run.RunID, "approve-staging")

	_, err := f.s.ExpireApprovalGate(f.ctx, gate, run.RunID, "approval timeout (168h)")
	if !errors.Is(err, ErrApprovalGateDecided) {
		t.Fatalf("err = %v, want ErrApprovalGateDecided", err)
	}
	// And critically: the run must be untouched, not half-cancelled.
	if st := f.stateOf(t, run.RunID); st.status == "canceled" {
		t.Fatalf("run was canceled despite the gate being approved under us")
	}
}

func TestExpireApprovalGate_TerminalRunIsRefused(t *testing.T) {
	f := newGateFixture(t, "expiry-idempotent")
	run := f.createRun(t, "main")
	gate := jobRunID(t, f, run.RunID, "approve-staging")
	backdateGate(t, f, run.RunID, "approve-staging", 8*24*time.Hour)

	if _, err := f.s.ExpireApprovalGate(f.ctx, gate, run.RunID, "approval timeout (168h)"); err != nil {
		t.Fatalf("first expire: %v", err)
	}
	// Second call: the run is terminal, so nothing to do. Idempotent rather
	// than an error the caller has to special-case beyond the sentinel.
	_, err := f.s.ExpireApprovalGate(f.ctx, gate, run.RunID, "approval timeout (168h)")
	if !errors.Is(err, ErrRunAlreadyTerminal) && !errors.Is(err, ErrApprovalGateDecided) {
		t.Fatalf("second expire err = %v, want a terminal/decided sentinel", err)
	}
}

// A gate that opts out with `timeout: never` must survive any server default —
// this is the compliance-window / scheduled-release escape hatch, and if the
// fleet default could override it the opt-out would be meaningless.
func TestResolveApprovalWindow_NeverBeatsServerDefault(t *testing.T) {
	f := newGateFixture(t, "expiry-never")

	// Re-apply the pipeline with an opted-out gate, then create a run off it
	// so the run's definition snapshot carries the sentinel.
	def := f.def
	jobs := make([]domain.Job, len(def.Jobs))
	copy(jobs, def.Jobs)
	for i := range jobs {
		if jobs[i].Name == "approve-staging" {
			spec := *jobs[i].Approval
			spec.Timeout = domain.ApprovalTimeoutNever
			jobs[i].Approval = &spec
		}
	}
	def.Jobs = jobs
	if _, err := f.s.ApplyProject(f.ctx, ApplyProjectInput{
		Slug: "expiry-never", Name: "expiry-never", Pipelines: []*domain.Pipeline{&def},
	}); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	run := f.createRun(t, "main")

	if _, ok, err := f.s.ResolveApprovalWindow(f.ctx, run.RunID, "approve-staging", time.Hour); err != nil || ok {
		t.Fatalf("never must beat the server default, got (%v, %v)", ok, err)
	}
	// A sibling gate that did NOT opt out still inherits the default.
	if d, ok, err := f.s.ResolveApprovalWindow(f.ctx, run.RunID, "approve-prod", time.Hour); err != nil || !ok || d != time.Hour {
		t.Fatalf("sibling gate = (%v, %v, %v), want (1h, true, nil)", d, ok, err)
	}
}

// Keyset paging must partition the queue exactly: no row served twice, no row
// skipped. The hard case is TIED timestamps — every gate of a run is stamped
// in one transaction, so siblings share awaiting_since to the microsecond. A
// timestamp-only cursor would either loop on a tie forever or jump the whole
// group; the id tiebreak is what makes the position total.
func TestListPendingApprovalGates_KeysetPagingIsExact(t *testing.T) {
	f := newGateFixture(t, "expiry-paging")

	// Three runs × two gates each = 6 candidates, with each run's pair sharing
	// a timestamp exactly (same materialisation tx).
	want := map[uuid.UUID]bool{}
	for i := 0; i < 3; i++ {
		run := f.createRun(t, "main")
		for _, gate := range []string{"approve-staging", "approve-prod"} {
			backdateGate(t, f, run.RunID, gate, time.Duration(i+1)*time.Hour)
			want[jobRunID(t, f, run.RunID, gate)] = true
		}
	}

	cutoff := time.Now().Add(-time.Minute)
	seen := map[uuid.UUID]int{}
	cursor := ApprovalGateCursor{}
	// Page size 1 is the meanest setting: every tie boundary becomes a page
	// boundary, so a broken cursor stalls or skips immediately.
	for pages := 0; pages < 50; pages++ {
		page, err := f.s.ListPendingApprovalGates(f.ctx, cutoff, cursor, 1)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(page) == 0 {
			break
		}
		for _, c := range page {
			seen[c.JobRunID]++
			cursor = cursor.Next(c)
		}
	}

	for id := range want {
		switch seen[id] {
		case 0:
			t.Fatalf("gate %s was skipped by paging", id)
		case 1: // exactly once
		default:
			t.Fatalf("gate %s served %d times — the cursor is not advancing past ties", id, seen[id])
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("saw %d gates, want %d", len(seen), len(want))
	}
}
