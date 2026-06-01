package scheduler_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// recordingFencer captures every FenceStaleSession the Reaper fires
// so tests can assert (a) which (agent, generation) pairs were
// fenced and (b) which ones were no-ops because the configured
// liveGeneration didn't match (the round-11 guard against killing
// a freshly-Registered successor session).
//
// Implements scheduler.SessionFencer.
type recordingFencer struct {
	mu sync.Mutex
	// liveGeneration is the "current generation per agent" map the
	// recorder pretends the in-memory SessionStore has. The Reaper
	// hands us (agentID, observedGeneration); we return true only
	// if liveGeneration[agentID] == observedGeneration (mirroring
	// SessionStore.FenceStaleSession's CAS). Tests set entries
	// here to simulate "successor already registered" scenarios.
	liveGeneration map[uuid.UUID]int64
	calls          []fencedCall
}

type fencedCall struct {
	AgentID    uuid.UUID
	Generation int64
	Result     grpcsrv.FenceResult
}

func newRecordingFencer() *recordingFencer {
	return &recordingFencer{liveGeneration: map[uuid.UUID]int64{}}
}

// setLiveGeneration tells the recorder what generation the in-memory
// session for `id` currently carries — i.e. what a real SessionStore
// would compare the observedGeneration against.
func (f *recordingFencer) setLiveGeneration(id uuid.UUID, gen int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.liveGeneration[id] = gen
}

func (f *recordingFencer) FenceStaleSession(agentID uuid.UUID, observedGeneration int64) grpcsrv.FenceResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	live, ok := f.liveGeneration[agentID]
	var result grpcsrv.FenceResult
	switch {
	case !ok:
		result = grpcsrv.FenceResultNoSession
	case live == observedGeneration:
		result = grpcsrv.FenceResultRevoked
	default:
		result = grpcsrv.FenceResultGenerationChanged
	}
	f.calls = append(f.calls, fencedCall{
		AgentID:    agentID,
		Generation: observedGeneration,
		Result:     result,
	})
	return result
}

func (f *recordingFencer) snapshot() []fencedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fencedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func TestReaper_Sweep_RequeuesOfflineAgentJobs(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	agentID := seedAgentRow(t, pool, "reaper-1")

	// Flip the compile job to running with this agent, then mark the agent
	// offline — the reaper should re-queue on the next sweep.
	if _, err := pool.Exec(ctx, `
		UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW()
		WHERE run_id=$2 AND name='compile'`, agentID, runID); err != nil {
		t.Fatalf("flip running: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agents SET status='offline' WHERE id=$1`, agentID); err != nil {
		t.Fatalf("agent offline: %v", err)
	}

	reaper := scheduler.NewReaper(s, quietLogger()).
		WithStaleness(10 * time.Second).
		WithMaxAttempts(3)
	reaper.Sweep(ctx)

	var (
		status     string
		pgAgentID  *uuid.UUID
		attempt    int32
	)
	_ = pool.QueryRow(ctx,
		`SELECT status, agent_id, attempt FROM job_runs WHERE run_id=$1 AND name='compile'`,
		runID,
	).Scan(&status, &pgAgentID, &attempt)
	if status != "queued" {
		t.Fatalf("status = %q, want queued", status)
	}
	if pgAgentID != nil {
		t.Fatalf("agent_id not cleared: %v", pgAgentID)
	}
	if attempt != 1 {
		t.Fatalf("attempt = %d, want 1", attempt)
	}
}

func TestReaper_Sweep_NoStaleJobsIsNoop(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	reaper := scheduler.NewReaper(s, quietLogger())
	// Empty DB — sweep must return without error and not panic.
	reaper.Sweep(context.Background())
}

// TestReaper_Sweep_FencesAgentBeforeNotify is the regression test
// for the round-10 HIGH: the reaper requeues stale jobs, but the
// scheduler picks agents purely by in-memory SessionStore capacity
// — without an explicit fence, a NOTIFY firing before the agent's
// session is revoked would let the same job get redispatched to
// the same stale session under a new attempt, corrupting the next
// attempt's row with the old attempt's eventual JobResult/logs.
//
// The reaper must: (1) requeue with notify=false, (2) fence each
// unique previous_agent via FenceStaleSession (CAS by generation
// — see TestReaper_Sweep_SkipsFenceWhenGenerationChanged), (3)
// NotifyRunQueued per unique run id — strictly in that order.
func TestReaper_Sweep_FencesAgentBeforeNotify(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	agentID := seedAgentRow(t, pool, "reaper-fence-1")

	if _, err := pool.Exec(ctx, `
		UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW()
		WHERE run_id=$2 AND name='compile'`, agentID, runID); err != nil {
		t.Fatalf("flip running: %v", err)
	}
	// Bump generation to a known non-zero value so the test exercises
	// the CAS path with realistic state (not the default-zero case).
	if _, err := pool.Exec(ctx, `UPDATE agents SET status='offline', session_generation=7 WHERE id=$1`, agentID); err != nil {
		t.Fatalf("agent offline: %v", err)
	}

	fencer := newRecordingFencer()
	fencer.setLiveGeneration(agentID, 7) // matches DB → fence will revoke
	reaper := scheduler.NewReaper(s, quietLogger()).
		WithStaleness(10 * time.Second).
		WithMaxAttempts(3).
		WithSessionFencer(fencer)
	reaper.Sweep(ctx)

	// Row was requeued.
	var status string
	_ = pool.QueryRow(ctx,
		`SELECT status FROM job_runs WHERE run_id=$1 AND name='compile'`,
		runID).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %q, want queued", status)
	}

	// Fence was called exactly once for our agent, with the snapshot
	// generation, and actually revoked (live matched).
	got := fencer.snapshot()
	if len(got) != 1 {
		t.Fatalf("fencer called %d times, want 1: %+v", len(got), got)
	}
	if got[0].AgentID != agentID {
		t.Fatalf("fenced agent = %v, want %v", got[0].AgentID, agentID)
	}
	if got[0].Generation != 7 {
		t.Fatalf("fenced generation = %d, want 7", got[0].Generation)
	}
	if got[0].Result != grpcsrv.FenceResultRevoked {
		t.Fatalf("fence result = %v, want FenceResultRevoked", got[0].Result)
	}
}

// TestReaper_Sweep_FencesEachAgentOnce — multiple stale rows from
// the SAME agent must collapse to one FenceStaleSession call, not
// N. Without dedup, a 16-job stale agent would close + reopen its
// session 16 times in one tick.
func TestReaper_Sweep_FencesEachAgentOnce(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	agentID := seedAgentRow(t, pool, "reaper-fence-2")

	// Flip BOTH compile and test jobs to running on the same agent.
	if _, err := pool.Exec(ctx, `
		UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW()
		WHERE run_id=$2 AND name IN ('compile', 'test')`, agentID, runID); err != nil {
		t.Fatalf("flip running: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agents SET status='offline' WHERE id=$1`, agentID); err != nil {
		t.Fatalf("agent offline: %v", err)
	}

	fencer := newRecordingFencer()
	fencer.setLiveGeneration(agentID, 0)
	reaper := scheduler.NewReaper(s, quietLogger()).
		WithStaleness(10 * time.Second).
		WithMaxAttempts(3).
		WithSessionFencer(fencer)
	reaper.Sweep(ctx)

	got := fencer.snapshot()
	if len(got) != 1 {
		t.Fatalf("fencer called %d times, want 1 (dedup): %+v", len(got), got)
	}
	if got[0].AgentID != agentID {
		t.Fatalf("fenced agent = %v, want %v", got[0].AgentID, agentID)
	}
}

// TestReaper_Sweep_SkipsFenceWhenGenerationChanged is the round-11
// HIGH regression test. Scenario: between ListStaleRunningJobs
// snapshotting `agents.session_generation = N` and the reaper's
// fence call, the agent re-Registered — bumping session_generation
// to N+1 and putting a NEW healthy session in place. Without the
// CAS, FenceStaleSession(agentID) would revoke the freshly-online
// successor's session, killing a clean stream for no reason. The
// fix: pass observedGeneration to the fencer; live != observed
// → no-op.
//
// We simulate this by:
//   1. Planting a stale row with agents.session_generation = 3
//      (the snapshot value the reaper will read).
//   2. Telling the recordingFencer the LIVE generation is 4
//      (the successor's value — distinct from the snapshot).
//   3. Asserting the row got requeued (DB work happens regardless)
//      AND the fence call returned false (Revoked: false) instead
//      of closing the successor.
func TestReaper_Sweep_SkipsFenceWhenGenerationChanged(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	agentID := seedAgentRow(t, pool, "reaper-fence-4")

	if _, err := pool.Exec(ctx, `
		UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW()
		WHERE run_id=$2 AND name='compile'`, agentID, runID); err != nil {
		t.Fatalf("flip running: %v", err)
	}
	// Snapshot value the reaper will read at SELECT time.
	if _, err := pool.Exec(ctx, `UPDATE agents SET status='offline', session_generation=3 WHERE id=$1`, agentID); err != nil {
		t.Fatalf("agent offline: %v", err)
	}

	fencer := newRecordingFencer()
	// Successor scenario: the live SessionStore has generation 4 —
	// reaper's observedGeneration=3 must NOT match.
	fencer.setLiveGeneration(agentID, 4)

	reaper := scheduler.NewReaper(s, quietLogger()).
		WithStaleness(10 * time.Second).
		WithMaxAttempts(3).
		WithSessionFencer(fencer)
	reaper.Sweep(ctx)

	// DB requeue still happens — that's the snapshot-CAS path on the
	// row itself, unrelated to the in-memory session check.
	var status string
	_ = pool.QueryRow(ctx,
		`SELECT status FROM job_runs WHERE run_id=$1 AND name='compile'`,
		runID).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %q, want queued (DB requeue should still happen)", status)
	}

	got := fencer.snapshot()
	if len(got) != 1 {
		t.Fatalf("fencer called %d times, want 1: %+v", len(got), got)
	}
	if got[0].AgentID != agentID {
		t.Fatalf("fenced agent = %v, want %v", got[0].AgentID, agentID)
	}
	if got[0].Generation != 3 {
		t.Fatalf("fenced observed_generation = %d, want 3 (snapshot value)", got[0].Generation)
	}
	if got[0].Result == grpcsrv.FenceResultRevoked {
		t.Fatal("fence revoked the successor session — generation CAS failed to skip")
	}
	if got[0].Result != grpcsrv.FenceResultGenerationChanged {
		t.Fatalf("fence result = %v, want FenceResultGenerationChanged", got[0].Result)
	}
}

// TestReaper_Sweep_NilFencerStillRequeues — production wires the
// SessionStore as fencer, but tests and bootstrap configs may run
// with fencer=nil. Sweep must still complete the requeue (the DB
// snapshot CAS is the persistent defense; the fence is in-memory
// hardening). Regression guard against a "fencer == nil → panic"
// accident in the Sweep refactor.
func TestReaper_Sweep_NilFencerStillRequeues(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	agentID := seedAgentRow(t, pool, "reaper-fence-3")

	if _, err := pool.Exec(ctx, `
		UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW()
		WHERE run_id=$2 AND name='compile'`, agentID, runID); err != nil {
		t.Fatalf("flip running: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agents SET status='offline' WHERE id=$1`, agentID); err != nil {
		t.Fatalf("agent offline: %v", err)
	}

	// Explicit nil fencer (default) — must not panic, must still requeue.
	reaper := scheduler.NewReaper(s, quietLogger()).
		WithStaleness(10 * time.Second).
		WithMaxAttempts(3)
	reaper.Sweep(ctx)

	var status string
	_ = pool.QueryRow(ctx,
		`SELECT status FROM job_runs WHERE run_id=$1 AND name='compile'`,
		runID).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %q, want queued (nil-fencer must not block requeue)", status)
	}
}
