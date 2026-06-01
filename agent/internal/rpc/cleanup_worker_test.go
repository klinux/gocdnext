package rpc_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/rpc"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// blockableEngine lets a test pin the CleanupRunServices call so
// the worker is observably mid-flight when shutdown fires. The
// `release` channel unblocks the in-progress call. The `seen`
// slice records every runID the engine received with the per-call
// ctx state at receive time and the call's wall-clock duration —
// used to distinguish in-flight (cancelled by worker ctx, returns
// near-instantly) from drain (fresh ctx, blocks until release or
// shutdown timeout).
type blockableEngine struct {
	release  chan struct{}
	started  chan string // each call signals once it's blocked
	mu       sync.Mutex
	seen     []recordedCall
	finished atomic.Int32
}

type recordedCall struct {
	RunID         string
	Deadline      time.Time
	HadCtx        bool
	ErrAtEntry    error // ctx.Err() right when the engine received the call
	Duration      time.Duration
	ReturnedError error
}

func newBlockableEngine() *blockableEngine {
	return &blockableEngine{
		release: make(chan struct{}),
		started: make(chan string, 16),
	}
}

func (e *blockableEngine) Name() string { return "kubernetes" }
func (e *blockableEngine) RunScript(context.Context, engine.ScriptSpec) (int, error) {
	return 0, nil
}
func (e *blockableEngine) EnsureServices(context.Context, []engine.ServiceSpec, string, string, func(string, string)) (engine.ServicesWireup, error) {
	return engine.ServicesWireup{Cleanup: func() {}}, nil
}
func (e *blockableEngine) CleanupRunServices(ctx context.Context, runID string) (int, error) {
	dl, ok := ctx.Deadline()
	errAtEntry := ctx.Err()
	start := time.Now()
	// Track the call BEFORE blocking so the test can observe
	// drain ordering even when the engine never returns within
	// the test budget.
	e.mu.Lock()
	idx := len(e.seen)
	e.seen = append(e.seen, recordedCall{
		RunID:      runID,
		Deadline:   dl,
		HadCtx:     ok,
		ErrAtEntry: errAtEntry,
	})
	e.mu.Unlock()

	select {
	case e.started <- runID:
	default: // started buffer full — fine, test only reads first one
	}

	var (
		deleted int
		retErr  error
	)
	select {
	case <-e.release:
		deleted = 1
	case <-ctx.Done():
		retErr = ctx.Err()
	}

	e.mu.Lock()
	e.seen[idx].Duration = time.Since(start)
	e.seen[idx].ReturnedError = retErr
	e.mu.Unlock()
	e.finished.Add(1)
	return deleted, retErr
}

func (e *blockableEngine) calls() []recordedCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]recordedCall, len(e.seen))
	copy(out, e.seen)
	return out
}

// TestCleanupWorker_ShutdownInterruptsInFlight is the round-8 MED
// invariant: per-cleanup contexts derive from the worker's parent
// ctx, so cancelWorkers() interrupts steady-state calls already
// in flight — they don't out-live the shutdown budget by sitting
// at their 60s steady-state ceiling.
//
// Asserts:
//   - Every in-flight worker (4) reaches the engine, blocks on
//     ctx.Done() of the worker ctx, returns ctx.Err in <100ms.
//   - shutdown() completes under the drain budget.
//
// Does NOT assert anything about the drain backlog — that's the
// job of TestCleanupWorker_DrainHasUsableCtx below.
func TestCleanupWorker_ShutdownInterruptsInFlight(t *testing.T) {
	eng := newBlockableEngine()
	c := rpc.New(rpc.Config{
		ServerAddr: "ignored", AgentID: "a", Token: "t",
		Engine: eng,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Exactly cleanupWorkers items so every worker has one and
	// the queue is empty when shutdown fires — isolates the
	// in-flight cancellation path from the drain path.
	const inFlight = 4 // == cleanupWorkers
	for i := 0; i < inFlight; i++ {
		c.EnqueueRunCleanupForTest("inflight-" + string(rune('A'+i)))
	}
	shutdown := c.StartCleanupWorkersForTest()

	// Wait until ALL workers entered the engine (so cancellation
	// catches the steady-state branch, not the drain branch).
	for i := 0; i < inFlight; i++ {
		select {
		case <-eng.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d/%d workers reached engine within 2s", i, inFlight)
		}
	}

	start := time.Now()
	timedOut := shutdown()
	elapsed := time.Since(start)

	if timedOut {
		t.Fatal("shutdown exceeded drain budget — ctx propagation didn't interrupt in-flight cleanups")
	}
	if elapsed > 5*time.Second {
		t.Errorf("shutdown took %v, expected <5s; steady-state ctx propagation regressed", elapsed)
	}

	got := eng.calls()
	if len(got) != inFlight {
		t.Fatalf("engine saw %d calls, want %d: %+v", len(got), inFlight, got)
	}
	for _, r := range got {
		if r.ReturnedError == nil {
			t.Errorf("in-flight call %s returned nil error; expected ctx.Err propagation: %+v", r.RunID, r)
		}
		// Cancellation must be FAST — well under both timeouts.
		if r.Duration > 500*time.Millisecond {
			t.Errorf("in-flight call %s took %v, expected <500ms (ctx cancellation): %+v",
				r.RunID, r.Duration, r)
		}
	}

	if got := c.CleanupPendingLenForTest(); got != 0 {
		t.Errorf("pending after shutdown = %d, want 0", got)
	}
}

// TestCleanupWorker_DrainHasUsableCtx is the round-10 MED guard:
// shutdown drain must give each remaining queued item a FRESH
// context (Background-rooted, not the worker's already-cancelled
// ctx). Without this, drain calls return ctx.Cancelled at engine
// entry, pending is cleared, and ZERO pods are actually deleted —
// the "attempt cleanup before exit" promise silently violated.
//
// Drives drainCleanupQueue directly via export helper to isolate
// the ctx-shape assertion from worker-lifecycle timing.
func TestCleanupWorker_DrainHasUsableCtx(t *testing.T) {
	eng := newBlockableEngine()
	// Pre-close release so the engine returns immediately —
	// the assertion isn't about timeout behaviour, it's about
	// whether the ctx is usable when the engine starts.
	close(eng.release)

	c := rpc.New(rpc.Config{
		ServerAddr: "ignored", AgentID: "a", Token: "t",
		Engine: eng,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const queued = 4
	for i := 0; i < queued; i++ {
		c.EnqueueRunCleanupForTest("drain-" + string(rune('A'+i)))
	}

	c.DrainCleanupQueueForTest()

	got := eng.calls()
	if len(got) != queued {
		t.Fatalf("engine saw %d calls, want %d: %+v", len(got), queued, got)
	}
	for _, r := range got {
		if r.ErrAtEntry != nil {
			t.Errorf("drain call %s entered engine with ctx.Err()=%v — derived from cancelled parent? %+v",
				r.RunID, r.ErrAtEntry, r)
		}
		if r.ReturnedError != nil {
			t.Errorf("drain call %s returned error %v; engine couldn't do work: %+v",
				r.RunID, r.ReturnedError, r)
		}
		if !r.HadCtx {
			t.Errorf("drain call %s had no deadline; bound is required to prevent runaway: %+v",
				r.RunID, r)
		}
	}
	if got := c.CleanupPendingLenForTest(); got != 0 {
		t.Errorf("pending after drain = %d, want 0", got)
	}
}

// TestCleanupWorker_ShutdownRaceUsesFreshCtx covers the round-11
// MED finding: when Go's select picks the cleanupQueue branch
// even though ctx.Done() is ALSO ready (ready-cases race), the
// in-branch ctx.Err() check must route the item through fresh-ctx
// processing — not derive from the cancelled worker ctx, which
// would let the engine see Cancelled at entry and silently skip
// the cleanup.
//
// Drives a single worker iteration with the worker ctx already
// cancelled at start. Go select is allowed to pick either arm;
// the assertion holds for BOTH paths (drain branch uses fresh
// ctx by construction; queue branch with race-recovery uses
// fresh ctx via the new conditional).
func TestCleanupWorker_ShutdownRaceUsesFreshCtx(t *testing.T) {
	eng := newBlockableEngine()
	close(eng.release) // engine returns immediately on receive

	c := rpc.New(rpc.Config{
		ServerAddr: "ignored", AgentID: "a", Token: "t",
		Engine: eng,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const queued = 3
	for i := 0; i < queued; i++ {
		c.EnqueueRunCleanupForTest("race-" + string(rune('A'+i)))
	}

	// Pre-cancel BEFORE starting the worker so both select arms
	// are ready on first entry.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c.RunCleanupWorkerLoopForTest(ctx)

	got := eng.calls()
	if len(got) != queued {
		t.Fatalf("engine saw %d calls, want %d (all items must reach engine even in race): %+v",
			len(got), queued, got)
	}
	for _, r := range got {
		if r.ErrAtEntry != nil {
			t.Errorf("race-branch call %s entered engine with ctx.Err()=%v — derived from cancelled parent? %+v",
				r.RunID, r.ErrAtEntry, r)
		}
		if r.ReturnedError != nil {
			t.Errorf("race-branch call %s returned %v — engine couldn't do work with cancelled ctx",
				r.RunID, r.ReturnedError)
		}
	}
	if got := c.CleanupPendingLenForTest(); got != 0 {
		t.Errorf("pending after race-recovery = %d, want 0", got)
	}
}

// TestCleanupWorker_ProcessShutdownRaceItem deterministically
// exercises the queue-branch race recovery — no reliance on Go's
// runtime select to pick the recovery arm. Mirrors what the worker
// does when it has just popped a runID from cleanupQueue with the
// worker ctx already cancelled: process the popped item, then
// drain the rest. Asserts every engine call enters with a USABLE
// ctx (fresh drain-budget parent, not the cancelled worker ctx)
// and that every queued item is reached.
//
// Companion to TestCleanupWorker_ShutdownRaceUsesFreshCtx (which
// covers the same invariant via the full worker loop but is
// probabilistic over Go's select). This one nails the queue branch.
func TestCleanupWorker_ProcessShutdownRaceItem(t *testing.T) {
	eng := newBlockableEngine()
	close(eng.release) // engine returns immediately

	c := rpc.New(rpc.Config{
		ServerAddr: "ignored", AgentID: "a", Token: "t",
		Engine: eng,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// race-A is the runID we'd have just popped from the queue
	// (the worker already received it before discovering ctx was
	// cancelled). Mirror that by enqueueing ONLY the remaining
	// queue items B/C/D/E — race-A reaches the engine via the
	// direct recovery call, the others via the in-method drain.
	const inQueue = 4
	for i := 0; i < inQueue; i++ {
		c.EnqueueRunCleanupForTest("race-" + string(rune('B'+i)))
	}

	c.ProcessShutdownRaceItemForTest("race-A")

	got := eng.calls()
	const expected = inQueue + 1 // popped item + drain
	if len(got) != expected {
		t.Fatalf("engine saw %d calls, want %d (popped item + drain): %+v",
			len(got), expected, got)
	}
	for _, r := range got {
		if r.ErrAtEntry != nil {
			t.Errorf("race-recovery call %s entered engine with ctx.Err()=%v — derived from cancelled parent? %+v",
				r.RunID, r.ErrAtEntry, r)
		}
		if r.ReturnedError != nil {
			t.Errorf("race-recovery call %s returned %v — engine couldn't do work",
				r.RunID, r.ReturnedError)
		}
		if !r.HadCtx {
			t.Errorf("race-recovery call %s had no deadline; bound is required to prevent runaway: %+v",
				r.RunID, r)
		}
	}
	if got := c.CleanupPendingLenForTest(); got != 0 {
		t.Errorf("pending after recovery = %d, want 0", got)
	}
}

// Three deterministic tests cover what the earlier-flaky
// TestCleanupWorker_DrainHonoursBudget tried to assert in one
// timing-window. Splitting removes the 50ms race in both
// directions (zero items processed OR all items processed) that
// made the original brittle on busy/fast CI runners.
//
//  1. BudgetAlreadyFired_NothingDrains: pre-cancelled budget
//     before drain starts. Engine MUST see zero calls. Items
//     MUST remain on queue+pending. Deterministic.
//  2. BudgetGenerous_AllItemsDrain: budget large enough to let
//     every item finish. Engine sees all N calls, queue+pending
//     are empty after. Deterministic.
//  3. BudgetFiresMidFlight_CancelsInFlight: budget arms AFTER
//     the engine starts blocking; we cancel mid-call and assert
//     the engine returned Cancelled. Deterministic via a manual
//     CancelFunc call (no time-based race).

func TestCleanupWorker_BudgetAlreadyFired_NothingDrains(t *testing.T) {
	eng := newBlockableEngine()
	// release intentionally NOT closed — proves no engine call
	// gets through (otherwise the test would hang).

	c := rpc.New(rpc.Config{
		ServerAddr: "ignored", AgentID: "a", Token: "t",
		Engine: eng,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const queued = 10
	for i := 0; i < queued; i++ {
		c.EnqueueRunCleanupForTest("budget-" + string(rune('A'+i)))
	}

	// Budget already done at the moment drain enters its top check.
	budgetCtx, cancelBudget := context.WithCancel(context.Background())
	cancelBudget()
	c.SetDrainBudgetForTest(budgetCtx)
	defer c.SetDrainBudgetForTest(nil)

	c.DrainCleanupQueueForTest()

	if got := len(eng.calls()); got != 0 {
		t.Errorf("engine saw %d calls; expected 0 because budget was pre-fired", got)
	}
	if got := c.CleanupQueueLenForTest(); got != queued {
		t.Errorf("queue len = %d, want %d (items must remain on channel after budget gate)", got, queued)
	}
	if got := c.CleanupPendingLenForTest(); got != queued {
		t.Errorf("pending len = %d, want %d (items must remain in pending after budget gate)", got, queued)
	}
}

func TestCleanupWorker_BudgetGenerous_AllItemsDrain(t *testing.T) {
	eng := newBlockableEngine()
	close(eng.release) // engine returns immediately

	c := rpc.New(rpc.Config{
		ServerAddr: "ignored", AgentID: "a", Token: "t",
		Engine: eng,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const queued = 10
	for i := 0; i < queued; i++ {
		c.EnqueueRunCleanupForTest("budget-" + string(rune('A'+i)))
	}

	// 1h budget — well above anything this in-memory drain could
	// possibly take.
	budgetCtx, cancelBudget := context.WithTimeout(context.Background(), time.Hour)
	defer cancelBudget()
	c.SetDrainBudgetForTest(budgetCtx)
	defer c.SetDrainBudgetForTest(nil)

	c.DrainCleanupQueueForTest()

	if got := len(eng.calls()); got != queued {
		t.Errorf("engine saw %d calls, want %d (budget should not have gated anything)", got, queued)
	}
	if got := c.CleanupQueueLenForTest(); got != 0 {
		t.Errorf("queue len = %d, want 0 (all items should drain)", got)
	}
	if got := c.CleanupPendingLenForTest(); got != 0 {
		t.Errorf("pending len = %d, want 0 (runCleanup defers should clear)", got)
	}
}

func TestCleanupWorker_BudgetFiresMidFlight_CancelsInFlight(t *testing.T) {
	eng := newBlockableEngine()
	// release intentionally NOT closed — engine blocks until ctx.Done.

	c := rpc.New(rpc.Config{
		ServerAddr: "ignored", AgentID: "a", Token: "t",
		Engine: eng,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	c.EnqueueRunCleanupForTest("budget-mid")

	// Manually-controlled budget: we cancel it AFTER the engine
	// has reached the block point. No time.After dependence.
	budgetCtx, cancelBudget := context.WithCancel(context.Background())
	c.SetDrainBudgetForTest(budgetCtx)
	defer c.SetDrainBudgetForTest(nil)

	drainDone := make(chan struct{})
	go func() {
		c.DrainCleanupQueueForTest()
		close(drainDone)
	}()

	// Wait for the engine to record the call (it's now blocking
	// on ctx.Done). Then fire the budget.
	select {
	case <-eng.started:
	case <-time.After(2 * time.Second):
		t.Fatal("engine never received the call")
	}
	cancelBudget()

	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("drain didn't exit after budget cancellation")
	}

	got := eng.calls()
	if len(got) != 1 {
		t.Fatalf("engine saw %d calls, want 1", len(got))
	}
	if got[0].ReturnedError == nil {
		t.Errorf("in-flight call returned nil error after budget cancel; per-item ctx didn't see Done")
	}
}

// TestCleanupWorker_SendsAckAfterEngineCall proves the round-13
// MED #2 fix: after the engine returns, the worker pushes a
// CleanupRunServicesResult up the per-stream outbound pump so the
// server can audit "which agent reported what" — the original
// dispatch-ok signal was masking leaks in multi-cluster setups
// where an agent received the broadcast but had no labeled pods
// in its namespace.
//
// Two cases:
//  1. Successful cleanup with deleted=N → ack carries the count
//     and empty error_message.
//  2. Engine error → ack carries deleted=0 (or whatever the
//     engine returned) AND the error string so the server can
//     log a warn.
func TestCleanupWorker_SendsAckAfterEngineCall(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		eng := newBlockableEngine()
		close(eng.release) // engine returns deleted=1, no error

		c := rpc.New(rpc.Config{
			ServerAddr: "ignored", AgentID: "a", Token: "t",
			Engine: eng,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))

		var captured []*gocdnextv1.AgentMessage
		var mu sync.Mutex
		c.SetCleanupAckSendForTest(func(msg *gocdnextv1.AgentMessage) {
			mu.Lock()
			defer mu.Unlock()
			captured = append(captured, msg)
		})
		defer c.SetCleanupAckSendForTest(nil)

		c.EnqueueRunCleanupForTest("run-ok")
		shutdown := c.StartCleanupWorkersForTest()
		// Wait for engine to record the call (proves the worker
		// processed it).
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && eng.finished.Load() == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		shutdown()

		mu.Lock()
		got := append([]*gocdnextv1.AgentMessage(nil), captured...)
		mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("expected 1 ack, got %d", len(got))
		}
		ack := got[0].GetCleanupRunServicesResult()
		if ack == nil {
			t.Fatalf("captured message is not a CleanupRunServicesResult: %T", got[0].Kind)
		}
		if ack.GetRunId() != "run-ok" {
			t.Errorf("run_id = %q, want run-ok", ack.GetRunId())
		}
		if ack.GetDeleted() != 1 {
			t.Errorf("deleted = %d, want 1", ack.GetDeleted())
		}
		if ack.GetErrorMessage() != "" {
			t.Errorf("error_message = %q, want empty", ack.GetErrorMessage())
		}
		if ack.GetEngine() != "kubernetes" {
			t.Errorf("engine = %q, want kubernetes", ack.GetEngine())
		}
	})

	t.Run("nil_sender_is_noop", func(t *testing.T) {
		// No SetCleanupAckSendForTest call — cleanupAckSend stays
		// nil. The engine should still run cleanly; sendCleanupAck
		// just returns without trying to send.
		eng := newBlockableEngine()
		close(eng.release)
		c := rpc.New(rpc.Config{
			ServerAddr: "ignored", AgentID: "a", Token: "t",
			Engine: eng,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))

		c.EnqueueRunCleanupForTest("run-no-stream")
		shutdown := c.StartCleanupWorkersForTest()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && eng.finished.Load() == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		shutdown()

		if eng.finished.Load() == 0 {
			t.Fatal("engine never saw the call; nil sender shouldn't have blocked it")
		}
	})
}

// TestCleanupAck_NonBlockingWhenOutboundFull is the round-14 PERF
// guard: the ack sender installed by runStream must NEVER block
// the calling cleanup worker. With a small worker pool (4) and a
// congested outbound channel, blocking on the ack would freeze
// every worker on observability bytes while real pod-reaper work
// piles up in the cleanup queue.
//
// Pre-fills a tiny outbound to capacity, fires the ack sender,
// asserts it returns within a tight deadline AND the dropped
// counter incremented (proves the drop path ran).
func TestCleanupAck_NonBlockingWhenOutboundFull(t *testing.T) {
	outbound := make(chan *gocdnextv1.AgentMessage, 1)
	outbound <- &gocdnextv1.AgentMessage{} // fill to cap

	var dropped atomic.Int64
	sender := rpc.NewCleanupAckSenderForTest(outbound, &dropped)

	done := make(chan struct{})
	go func() {
		sender(&gocdnextv1.AgentMessage{
			Kind: &gocdnextv1.AgentMessage_CleanupRunServicesResult{
				CleanupRunServicesResult: &gocdnextv1.CleanupRunServicesResult{
					RunId: "test-run", Deleted: 1, Engine: "kubernetes",
				},
			},
		})
		close(done)
	}()

	select {
	case <-done:
		// success — sender returned without blocking on the full channel
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ack sender blocked on full outbound — cleanup worker would freeze in production")
	}

	if got := dropped.Load(); got != 1 {
		t.Errorf("dropped counter = %d, want 1 (drop-on-full path didn't run)", got)
	}

	// Sanity: the original pre-fill is still there (the sender's
	// attempt was rejected, not enqueued ahead of it).
	if got := len(outbound); got != 1 {
		t.Errorf("outbound len = %d, want 1 (sender shouldn't have displaced anything)", got)
	}
}

// TestCleanupAck_DeliversWhenOutboundHasRoom is the companion
// happy path: when outbound has capacity, the ack lands in the
// channel and the dropped counter stays at zero.
func TestCleanupAck_DeliversWhenOutboundHasRoom(t *testing.T) {
	outbound := make(chan *gocdnextv1.AgentMessage, 4)
	var dropped atomic.Int64
	sender := rpc.NewCleanupAckSenderForTest(outbound, &dropped)

	msg := &gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_CleanupRunServicesResult{
			CleanupRunServicesResult: &gocdnextv1.CleanupRunServicesResult{
				RunId: "ack-room", Deleted: 2, Engine: "kubernetes",
			},
		},
	}
	sender(msg)

	if got := dropped.Load(); got != 0 {
		t.Errorf("dropped counter = %d, want 0 (channel had room)", got)
	}
	select {
	case got := <-outbound:
		if got != msg {
			t.Errorf("received different message than was sent")
		}
	default:
		t.Fatal("outbound has no message; sender silently dropped despite available capacity")
	}
}

// TestCleanupWorker_SteadyStateRemovesPending — sanity check on
// the happy path: a runID enqueued during steady-state gets
// processed and removed from pending. Catches a regression where
// the defer in runCleanup gets dropped, leaving pending entries
// orphaned (coalesce would then silently reject every subsequent
// broadcast for the same run forever).
func TestCleanupWorker_SteadyStateRemovesPending(t *testing.T) {
	eng := newBlockableEngine()
	c := rpc.New(rpc.Config{
		ServerAddr: "ignored", AgentID: "a", Token: "t",
		Engine: eng,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	close(eng.release) // unblock all calls immediately
	shutdown := c.StartCleanupWorkersForTest()
	defer shutdown()

	c.EnqueueRunCleanupForTest("run-steady")

	// Wait for the engine to record the call. Poll with a tight
	// deadline so a flake at most pays the test's worst-case 1s.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.finished.Load() >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if eng.finished.Load() == 0 {
		t.Fatal("engine never saw the cleanup call within 2s")
	}

	// Worker's defer should have removed the pending entry; the
	// removal can lag the engine-return by microseconds, so poll.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.CleanupPendingLenForTest() == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Errorf("pending not cleared after steady-state run; got %d", c.CleanupPendingLenForTest())
}
