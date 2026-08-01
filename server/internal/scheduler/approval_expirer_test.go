package scheduler_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedTwoGates applies a pipeline whose two gates carry DIFFERENT windows:
// `hold` opts out with `never`, `ship` inherits the server default. Sibling
// gates disagreeing is the case a per-run cache gets wrong.
func seedTwoGates(t *testing.T, pool *pgxpool.Pool, slug string) (runID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	url := "https://github.com/org/" + slug
	fp := domain.GitFingerprint(url, "main")

	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"hold-stage", "ship-stage"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "hold", Stage: "hold-stage", Approval: &domain.ApprovalSpec{
					Required: 1, Timeout: domain.ApprovalTimeoutNever,
				}},
				{Name: "ship", Stage: "ship-stage", Approval: &domain.ApprovalSpec{
					Required: 1, // inherits the server default
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applied.Pipelines[0].PipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "abc", Branch: "main", Provider: "github", Delivery: "d", TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run.RunID
}

func gateStatus(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, name string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM job_runs WHERE run_id=$1 AND name=$2`, runID, name).Scan(&status); err != nil {
		t.Fatalf("gate status %q: %v", name, err)
	}
	return status
}

// Regression: the sweep memoises resolved windows, and keying that cache by
// RUN would let the first gate's answer decide every sibling's fate. Here
// `hold` says never and `ship` inherits a 1h default.
//
// The backdating is deliberately UNEQUAL — `hold` older than `ship` — because
// candidates arrive ORDER BY awaiting_since ASC. That pins `hold` as the first
// gate resolved, so a per-run cache deterministically stores its `never` and
// then wrongly spares `ship`, failing this test every run instead of half of
// them. With equal timestamps the ordering is indeterminate and the bug would
// slip through intermittently.
func TestApprovalExpirer_SiblingGatesResolveIndependently(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID := seedTwoGates(t, pool, "expirer-siblings")
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET awaiting_since = NOW() - INTERVAL '72 hours'
		 WHERE run_id=$1 AND name='hold' AND status='awaiting_approval'`, runID); err != nil {
		t.Fatalf("backdate hold: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET awaiting_since = NOW() - INTERVAL '48 hours'
		 WHERE run_id=$1 AND name='ship' AND status='awaiting_approval'`, runID); err != nil {
		t.Fatalf("backdate ship: %v", err)
	}

	before := testutil.ToFloat64(metrics.ApprovalsExpired)
	scheduler.NewApprovalExpirer(s, time.Hour, quietLogger()).Sweep(ctx)

	if d := testutil.ToFloat64(metrics.ApprovalsExpired) - before; d != 1 {
		t.Fatalf("approvals_expired delta = %v, want 1 (the `ship` gate)", d)
	}

	var status string
	var reason *string
	if err := pool.QueryRow(ctx, `SELECT status, cancel_reason FROM runs WHERE id=$1`, runID).
		Scan(&status, &reason); err != nil {
		t.Fatalf("run state: %v", err)
	}
	if status != "canceled" {
		t.Fatalf("run status = %q, want canceled", status)
	}
	// The reason must name the gate that actually expired. Naming `hold`
	// would mean the never-opt-out was ignored.
	if reason == nil || !strings.Contains(*reason, `"ship"`) {
		t.Fatalf("cancel_reason = %v, want it to cite the ship gate", reason)
	}
	if reason != nil && strings.Contains(*reason, `"hold"`) {
		t.Fatalf("cancel_reason cites hold, whose `timeout: never` must be honoured: %q", *reason)
	}
}

// With no server default AND no per-gate window, nothing expires — the
// operator's fleet-wide kill switch must actually be a kill switch.
func TestApprovalExpirer_NoDefaultExpiresNothing(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID := seedTwoGates(t, pool, "expirer-disabled")
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET awaiting_since = NOW() - INTERVAL '400 days'
		 WHERE run_id=$1 AND status='awaiting_approval'`, runID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	before := testutil.ToFloat64(metrics.ApprovalsExpired)
	scheduler.NewApprovalExpirer(s, 0, quietLogger()).Sweep(ctx)
	if d := testutil.ToFloat64(metrics.ApprovalsExpired) - before; d != 0 {
		t.Fatalf("approvals_expired delta = %v, want 0 with expiry disabled", d)
	}
	if got := gateStatus(t, pool, runID, "ship"); got != "awaiting_approval" {
		t.Fatalf("ship gate = %q, want still awaiting", got)
	}
}

// A gate inside its window is left alone — the sweep must not cancel on the
// mere fact that a gate is old enough to be a candidate.
func TestApprovalExpirer_InsideWindowIsUntouched(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID := seedTwoGates(t, pool, "expirer-inside")
	// Past the 1m candidate cutoff, but far inside the 168h window.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET awaiting_since = NOW() - INTERVAL '10 minutes'
		 WHERE run_id=$1 AND status='awaiting_approval'`, runID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	before := testutil.ToFloat64(metrics.ApprovalsExpired)
	scheduler.NewApprovalExpirer(s, 168*time.Hour, quietLogger()).Sweep(ctx)
	if d := testutil.ToFloat64(metrics.ApprovalsExpired) - before; d != 0 {
		t.Fatalf("approvals_expired delta = %v, want 0 inside the window", d)
	}
	if got := gateStatus(t, pool, runID, "ship"); got != "awaiting_approval" {
		t.Fatalf("ship gate = %q, want still awaiting", got)
	}
}

// seedNeverGate applies a single-gate pipeline that opts out with
// `timeout: never`, and returns its run. Used to build a wall of
// never-expiring gates in front of an expirable one.
func seedNeverGate(t *testing.T, pool *pgxpool.Pool, slug string) uuid.UUID {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	url := "https://github.com/org/" + slug
	fp := domain.GitFingerprint(url, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"gate"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "hold", Stage: "gate", Approval: &domain.ApprovalSpec{
				Required: 1, Timeout: domain.ApprovalTimeoutNever,
			}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply %s: %v", slug, err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applied.Pipelines[0].PipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "abc", Branch: "main", Provider: "github", Delivery: "d", TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("create run %s: %v", slug, err)
	}
	return run.RunID
}

// Starvation regression. A wall of OLDER `never` gates must not hide a NEWER
// expirable one. The page size is squeezed to 2 so the wall spans several
// pages: with a single capped query — or with paging that restarts from the
// top every sweep — the expirable gate sits past the cap and is never reached,
// which is exactly the reported HIGH.
//
// The wall is deliberately older than the target so ORDER BY awaiting_since
// ASC puts every never-gate strictly in front of it.
func TestApprovalExpirer_NeverGatesDoNotStarveAnExpirableOne(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	const wall = 7
	for i := 0; i < wall; i++ {
		runID := seedNeverGate(t, pool, fmt.Sprintf("starve-never-%d", i))
		if _, err := pool.Exec(ctx,
			`UPDATE job_runs SET awaiting_since = NOW() - INTERVAL '90 days'
			 WHERE run_id=$1 AND status='awaiting_approval'`, runID); err != nil {
			t.Fatalf("backdate wall %d: %v", i, err)
		}
	}
	// The victim: newer than the whole wall, but well past a 1h window.
	target := seedTwoGates(t, pool, "starve-target")
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET awaiting_since = NOW() - INTERVAL '10 hours'
		 WHERE run_id=$1 AND status='awaiting_approval'`, target); err != nil {
		t.Fatalf("backdate target: %v", err)
	}

	before := testutil.ToFloat64(metrics.ApprovalsExpired)
	// pageSize 2 forces multiple pages across a 7-gate wall; scanCap high
	// enough to reach the target within this sweep.
	scheduler.NewApprovalExpirer(s, time.Hour, quietLogger()).
		WithLimits(2, 100, 50).
		Sweep(ctx)

	if d := testutil.ToFloat64(metrics.ApprovalsExpired) - before; d != 1 {
		t.Fatalf("approvals_expired delta = %v, want 1 — the expirable gate is starved behind the never wall", d)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, target).Scan(&status); err != nil {
		t.Fatalf("target state: %v", err)
	}
	if status != "canceled" {
		t.Fatalf("target run = %q, want canceled", status)
	}
}

// The scan ceiling must DEFER, never DROP. With scanCap below the wall size the
// first sweep can't reach the target — but the cursor persists, so a later
// sweep resumes past the wall and gets it. A cursor that reset each sweep would
// re-walk the same prefix forever and never expire anything.
func TestApprovalExpirer_CursorResumesAcrossSweeps(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	const wall = 6
	for i := 0; i < wall; i++ {
		runID := seedNeverGate(t, pool, fmt.Sprintf("resume-never-%d", i))
		if _, err := pool.Exec(ctx,
			`UPDATE job_runs SET awaiting_since = NOW() - INTERVAL '90 days'
			 WHERE run_id=$1 AND status='awaiting_approval'`, runID); err != nil {
			t.Fatalf("backdate wall %d: %v", i, err)
		}
	}
	target := seedTwoGates(t, pool, "resume-target")
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET awaiting_since = NOW() - INTERVAL '10 hours'
		 WHERE run_id=$1 AND status='awaiting_approval'`, target); err != nil {
		t.Fatalf("backdate target: %v", err)
	}

	before := testutil.ToFloat64(metrics.ApprovalsExpired)
	// scanCap 2 per sweep against a 6-gate wall: the target is unreachable in
	// one pass by construction.
	e := scheduler.NewApprovalExpirer(s, time.Hour, quietLogger()).WithLimits(2, 2, 50)

	e.Sweep(ctx)
	if d := testutil.ToFloat64(metrics.ApprovalsExpired) - before; d != 0 {
		t.Fatalf("first sweep expired %v, want 0 (target is past the scan cap)", d)
	}
	// Enough sweeps to walk the wall and reach the target. Each resumes from
	// the cursor; a reset-every-sweep implementation never gets here.
	for i := 0; i < 6; i++ {
		e.Sweep(ctx)
	}
	if d := testutil.ToFloat64(metrics.ApprovalsExpired) - before; d != 1 {
		t.Fatalf("after resuming sweeps expired = %v, want 1 — the cursor is not carrying across sweeps", d)
	}
}

// The per-sweep cancel ceiling must DEFER the next candidate, not skip it. The
// cursor may only advance past rows the sweep actually judged: advancing it for
// the row that tripped the ceiling would drop that gate until the cursor wrapped
// the entire queue.
//
// Two expirable gates land in ONE page with perSweepLimit=1, so the second is
// exactly the row at the boundary. The immediately-following sweep must get it.
func TestApprovalExpirer_CancelLimitDefersRatherThanSkips(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// Two independent runs, each with one expirable gate, both well past a 1h
	// window and both inside a single 10-row page.
	a := seedTwoGates(t, pool, "defer-a")
	b := seedTwoGates(t, pool, "defer-b")
	for _, runID := range []uuid.UUID{a, b} {
		if _, err := pool.Exec(ctx,
			`UPDATE job_runs SET awaiting_since = NOW() - INTERVAL '10 hours'
			 WHERE run_id=$1 AND name='ship' AND status='awaiting_approval'`, runID); err != nil {
			t.Fatalf("backdate ship: %v", err)
		}
		// Park the `never` siblings far in the future of the window so they
		// stay candidates but never expire, keeping the page mixed.
		if _, err := pool.Exec(ctx,
			`UPDATE job_runs SET awaiting_since = NOW() - INTERVAL '11 hours'
			 WHERE run_id=$1 AND name='hold' AND status='awaiting_approval'`, runID); err != nil {
			t.Fatalf("backdate hold: %v", err)
		}
	}

	before := testutil.ToFloat64(metrics.ApprovalsExpired)
	e := scheduler.NewApprovalExpirer(s, time.Hour, quietLogger()).WithLimits(10, 100, 1)

	e.Sweep(ctx)
	if d := testutil.ToFloat64(metrics.ApprovalsExpired) - before; d != 1 {
		t.Fatalf("first sweep expired %v, want exactly 1 (the per-sweep ceiling)", d)
	}
	// The very next sweep must pick up the deferred one. If the cursor had
	// advanced past it, this stays 1 until the queue wraps.
	e.Sweep(ctx)
	if d := testutil.ToFloat64(metrics.ApprovalsExpired) - before; d != 2 {
		t.Fatalf("second sweep total = %v, want 2 — the ceiling skipped a gate instead of deferring it", d)
	}

	for _, runID := range []uuid.UUID{a, b} {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&status); err != nil {
			t.Fatalf("run state: %v", err)
		}
		if status != "canceled" {
			t.Fatalf("run %s = %q, want canceled", runID, status)
		}
	}
}
