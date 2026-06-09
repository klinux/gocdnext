package rpc

import (
	"context"
	"sync/atomic"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// EnqueueRunCleanupForTest is the test-only handle into the
// coalescing dispatcher. Production code reaches it through the
// recv-loop's ServerMessage_CleanupRunServices switch — exposing
// it here keeps queue/coalesce/saturation tests in the _test
// package without making the production surface public.
func (c *Client) EnqueueRunCleanupForTest(runID string) {
	c.enqueueRunCleanup(runID)
}

// CleanupPendingLenForTest reports the size of the in-progress /
// queued set so tests can assert coalesce semantics
// (re-enqueuing the same runID doesn't grow the set).
func (c *Client) CleanupPendingLenForTest() int {
	c.cleanupMu.Lock()
	defer c.cleanupMu.Unlock()
	return len(c.cleanupPending)
}

// CleanupQueueLenForTest reports the number of runIDs currently
// buffered awaiting a worker. Combined with PendingLen, lets the
// saturation test verify "dropped on full queue" without poking
// at internals directly.
func (c *Client) CleanupQueueLenForTest() int {
	return len(c.cleanupQueue)
}

// CleanupQueueCapForTest exposes the buffered-channel capacity so
// the saturation test can fill exactly cap+1 items without
// hard-coding the constant.
func (c *Client) CleanupQueueCapForTest() int {
	return cap(c.cleanupQueue)
}

// RunCleanupWorkerLoopForTest is a single-call entry into the
// worker loop so a test can pre-cancel the ctx, pre-enqueue an
// item, and exercise the shutdown-race branch deterministically.
// The loop returns either via ctx.Done (drain mode) OR via the
// queue branch's "if ctx.Err() != nil" recovery — BOTH paths must
// use a fresh ctx for the engine call, so the test asserts that
// invariant regardless of which select arm Go picked.
//
// Adds to cleanupWG just like the production worker spawn so the
// loop's own defer balances out — callers don't need to manage it.
func (c *Client) RunCleanupWorkerLoopForTest(ctx context.Context) {
	c.cleanupWG.Add(1)
	c.cleanupWorkerLoop(ctx)
}

// DrainCleanupQueueForTest invokes the drain path directly so a
// test can prove the engine receives a USABLE context (fresh
// Background-rooted, not the worker's already-cancelled ctx).
// Callers pre-fill the queue, then call this — the test asserts
// the engine actually did work, didn't just see ctx.Err on entry.
func (c *Client) DrainCleanupQueueForTest() {
	c.drainCleanupQueue()
}

// ProcessShutdownRaceItemForTest exposes the factored queue-race
// recovery so a test can drive it deterministically — no reliance
// on Go's runtime select to pick the recovery arm. Pre-enqueue
// items, then call this with the runID you'd have just popped:
// it processes that item via the drain-budget parent and drains
// the rest of the queue identically to the production path.
func (c *Client) ProcessShutdownRaceItemForTest(runID string) {
	c.processShutdownRaceItem(runID)
}

// SetDrainBudgetForTest installs (or clears, with nil) the shared
// drain-budget ctx so tests can exercise the global-cap behaviour
// without driving Run() end-to-end. Without this, drain and
// race-recovery fall back to Background and behave as if no budget
// were active — fine for most unit tests; only needed when the
// test specifically wants to assert budget-fire propagation.
func (c *Client) SetDrainBudgetForTest(ctx context.Context) {
	if ctx == nil {
		c.drainBudget.Store(nil)
		return
	}
	c.drainBudget.Store(&ctx)
}

// SetCleanupAckSendForTest installs (or clears, with nil) the
// per-stream sendOutbound bridge so tests can capture the ack
// messages runCleanup emits after the engine call returns. Passing
// nil simulates "no stream connected" — the ack send becomes a
// no-op and the test asserts the engine path still runs cleanly.
func (c *Client) SetCleanupAckSendForTest(send func(*gocdnextv1.AgentMessage)) {
	if send == nil {
		c.cleanupAckSend.Store(nil)
		return
	}
	f := cleanupAckFunc(send)
	c.cleanupAckSend.Store(&f)
}

// NewCleanupAckSenderForTest exposes the production bridge builder
// so tests can drive the non-blocking + drop-on-full semantics
// without standing up a runStream. The dropped counter is the
// same atomic that logDroppedCleanupAcks reports in production.
func NewCleanupAckSenderForTest(outbound chan<- *gocdnextv1.AgentMessage, dropped *atomic.Int64) func(*gocdnextv1.AgentMessage) {
	return newCleanupAckSender(outbound, dropped)
}

// CacheProbeScriptForTest exposes the shell script the cache STORE
// path execs in the housekeeper. The v0.14.8 regression cover
// runs this against a real `sh` to verify "no paths exist" exits
// 0 (not 1, which pre-fix would surface as `cache store failed
// (probe paths: exit 1)`).
func CacheProbeScriptForTest() string { return cacheProbeScript() }

// StartCleanupWorkersForTest lets tests drive the worker pool
// directly without going through Run() (which dials + Registers,
// out of scope for unit tests of the worker behaviour). Returns
// a shutdown func that mirrors what Run()'s defer does: cancel
// workers, wait with the documented drain budget. The returned
// shutdown also reports whether the wait timed out, so tests can
// assert that drain completed within budget.
func (c *Client) StartCleanupWorkersForTest() (shutdown func() (timedOut bool)) {
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < cleanupWorkers; i++ {
		c.cleanupWG.Add(1)
		go c.cleanupWorkerLoop(ctx)
	}
	return func() bool {
		// Mirror Run()'s defer: install the shared drain-budget
		// BEFORE cancelling worker ctx so the per-item ctxs derive
		// from it, then wait on its Done as the budget ceiling.
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), cleanupShutdownDrainBudget)
		defer cancelDrain()
		c.drainBudget.Store(&drainCtx)
		cancel()
		done := make(chan struct{})
		go func() { c.cleanupWG.Wait(); close(done) }()
		select {
		case <-done:
			return false
		case <-drainCtx.Done():
			return true
		}
	}
}
