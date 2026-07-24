package rpc

import (
	"context"
	"sync/atomic"
	"time"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// This file holds the graceful-drain protocol (#178): the types the outbound
// FIFO carries for a drain, the per-identity suppression helpers, and the
// two-barrier rendezvous the drain runs when SIGTERM fires against a live
// stream. The stream/dispatch plumbing that DRIVES it lives in client.go.

// jobState tracks one in-flight job for graceful drain. `cancel` is created by
// the Client BEFORE the Execute goroutine, so the drain can reliably abort a job
// even before the runner registered its own canceller (the runner does that
// inside Execute — a window rn.Cancel would miss). `done` closes when Execute
// returns. `terminalSent` flips true after the job's JobResult crosses the wire
// — the FIFO-position fact the drain cutoff reads to tell "completed" from
// "still running".
type jobState struct {
	cancel       context.CancelFunc
	done         chan struct{}
	terminalSent atomic.Bool
}

// barrierState is a marker enqueued into the outbound FIFO. The sendLoop, on
// dequeuing it, closes `reached` (everything ahead of it is now sent) and blocks
// on `release` OR the stream ctx. runStream releases it after publishing the
// suppression policy. When closeSend is set (the FINAL barrier), the sendLoop
// CloseSend()s and returns after release — no more messages will be sent.
type barrierState struct {
	reached   chan struct{}
	release   chan struct{}
	closeSend bool
}

// DrainOutcome is what runStream reports after a graceful shutdown.
type DrainOutcome struct {
	Clean         bool // all jobs finished and their results were processed by the server
	Running       int  // jobs abandoned to the reaper (per-path count)
	FlushTimedOut bool // the flush protocol exceeded FlushTimeout
}

// jobOutputID returns the job_id of a job-scoped OUTPUT message (JobResult,
// Coverage, TestResults) and whether msg is one. These are the messages the
// drain suppresses for aborted jobs; a JobResult additionally flips terminalSent.
func jobOutputID(msg *gocdnextv1.AgentMessage) (string, bool) {
	switch k := msg.GetKind().(type) {
	case *gocdnextv1.AgentMessage_Result:
		return k.Result.GetJobId(), true
	case *gocdnextv1.AgentMessage_Coverage:
		return k.Coverage.GetJobId(), true
	case *gocdnextv1.AgentMessage_TestResults:
		return k.TestResults.GetJobId(), true
	}
	return "", false
}

// isJobResult reports whether msg is a terminal JobResult (the message whose
// wire delivery flips a job's terminalSent).
func isJobResult(msg *gocdnextv1.AgentMessage) bool {
	_, ok := msg.GetKind().(*gocdnextv1.AgentMessage_Result)
	return ok
}

// isSuppressed reports whether an OUTPUT message for job `id` should be dropped
// because its job is in the published suppression set (an aborted drain job).
func (c *Client) isSuppressed(id string) bool {
	if s := c.suppressed.Load(); s != nil {
		_, drop := (*s)[id]
		return drop
	}
	return false
}

// flushTimeout is the single deadline for the whole flush protocol.
func (c *Client) flushTimeout() time.Duration {
	if c.cfg.FlushTimeout > 0 {
		return c.cfg.FlushTimeout
	}
	return defaultFlushTimeout
}

// drain runs the graceful-shutdown protocol against the live stream and returns
// the outcome; the caller then cancels streamCtx to tear down. It stops new
// dispatch, tells the server (Draining), waits for in-flight jobs bounded by the
// budget, then either cleanly flushes (all done) or aborts survivors while
// preserving completed work.
func (c *Client) drain(streamCtx context.Context, outbound chan<- outboundItem, recvErrCh <-chan error) DrainOutcome {
	c.jobsMu.Lock()
	c.draining = true
	c.jobsMu.Unlock()
	c.sendDrainingFrame(streamCtx, outbound)

	if c.cfg.DrainBudget > 0 && c.waitJobs(c.cfg.DrainBudget) {
		c.log.Info("drain: all in-flight jobs finished within budget")
		return c.flushClean(streamCtx, outbound, recvErrCh)
	}
	return c.drainTimeout(streamCtx, outbound, recvErrCh)
}

// sendDrainingFrame enqueues one Draining message on a BOUNDED blocking send (not
// the drop-on-full observability path) — a dropped Draining lets the server keep
// dispatching, so a miss is logged loudly.
func (c *Client) sendDrainingFrame(streamCtx context.Context, outbound chan<- outboundItem) {
	msg := &gocdnextv1.AgentMessage{Kind: &gocdnextv1.AgentMessage_Draining{Draining: &gocdnextv1.Draining{}}}
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	select {
	case outbound <- outboundItem{msg: msg}:
	case <-t.C:
		c.log.Warn("drain: Draining frame not sent within 2s; server may keep dispatching")
	case <-streamCtx.Done():
		c.log.Warn("drain: stream closed before Draining frame sent")
	}
}

// waitJobs blocks until all in-flight jobs finish (jobsWG) or the budget expires;
// returns true on a clean finish.
func (c *Client) waitJobs(budget time.Duration) bool {
	done := make(chan struct{})
	go func() { c.jobsWG.Wait(); close(done) }()
	t := time.NewTimer(budget)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// flushClean is the CLEAN path (all jobs done): a single barrier flushes the
// buffered messages, CloseSend, then wait for the server EOF — the confirmation
// that every JobResult was processed (CompleteJob) before teardown.
func (c *Client) flushClean(streamCtx context.Context, outbound chan<- outboundItem, recvErrCh <-chan error) DrainOutcome {
	flushCtx, cancel := context.WithTimeout(context.Background(), c.flushTimeout())
	defer cancel()

	b := &barrierState{reached: make(chan struct{}), release: make(chan struct{}), closeSend: true}
	if !c.enqueueBarrier(streamCtx, outbound, b, flushCtx) {
		return DrainOutcome{FlushTimedOut: true}
	}
	close(b.release) // sendLoop CloseSends after this barrier
	select {
	case <-recvErrCh:
		return DrainOutcome{Clean: true}
	case <-flushCtx.Done():
		return DrainOutcome{FlushTimedOut: true}
	}
}

// drainTimeout is the TIMEOUT path (budget expired or <=0): the two-barrier
// rendezvous. It preserves jobs whose result crossed the FIFO before the cutoff
// and aborts the rest, all under ONE shared flush deadline.
func (c *Client) drainTimeout(streamCtx context.Context, outbound chan<- outboundItem, recvErrCh <-chan error) DrainOutcome {
	flushCtx, cancel := context.WithTimeout(context.Background(), c.flushTimeout())
	defer cancel()

	// Snapshot candidates BEFORE enqueuing the cutoff barrier (never recomputed) —
	// the fix for a job finishing DURING the backlog drain and leaving `running`.
	c.jobsMu.Lock()
	candidates := make(map[string]*jobState, len(c.running))
	for id, st := range c.running {
		candidates[id] = st
	}
	c.jobsMu.Unlock()

	cutoff := &barrierState{reached: make(chan struct{}), release: make(chan struct{})}
	if !c.enqueueBarrier(streamCtx, outbound, cutoff, flushCtx) {
		return c.drainFailSafe(candidates)
	}

	// suppressed = candidates whose JobResult did NOT cross before the cutoff.
	suppressed := make(map[string]struct{})
	for id, st := range candidates {
		if !st.terminalSent.Load() {
			suppressed[id] = struct{}{}
		}
	}
	c.suppressed.Store(&suppressed) // publish BEFORE releasing the barrier
	close(cutoff.release)

	// Abort survivors via the Client-owned per-job cancel (reliable even for a
	// just-dispatched job), then wait for EVERY candidate's cleanup so no residual
	// pod is left. Bounded by the shared flush deadline.
	for id := range suppressed {
		candidates[id].cancel()
	}
	c.waitDone(candidates, flushCtx)

	running := len(suppressed)
	if running > 0 {
		c.log.Warn("drain: budget expired; abandoning in-flight jobs to the reaper", "jobs", running)
	}

	final := &barrierState{reached: make(chan struct{}), release: make(chan struct{}), closeSend: true}
	if !c.enqueueBarrier(streamCtx, outbound, final, flushCtx) {
		return DrainOutcome{Running: running, FlushTimedOut: true}
	}
	close(final.release)
	select {
	case <-recvErrCh:
		return DrainOutcome{Running: running}
	case <-flushCtx.Done():
		return DrainOutcome{Running: running, FlushTimedOut: true}
	}
}

// drainFailSafe handles a cutoff barrier that was never reached (a wedged
// sendLoop): it does NOT guess survivors — it publishes suppress-all
// (candidates ∪ current running) BEFORE cancelling, so a post-cutoff result can
// never escape even if the sendLoop drains it on the way out. The caller cancels
// the stream next.
func (c *Client) drainFailSafe(candidates map[string]*jobState) DrainOutcome {
	c.jobsMu.Lock()
	all := make(map[string]*jobState, len(candidates)+len(c.running))
	for id, st := range candidates {
		all[id] = st
	}
	for id, st := range c.running {
		all[id] = st
	}
	c.jobsMu.Unlock()

	ids := make(map[string]struct{}, len(all))
	for id := range all {
		ids[id] = struct{}{}
	}
	c.suppressed.Store(&ids)
	for _, st := range all {
		st.cancel()
	}
	c.log.Warn("drain: flush deadline hit before cutoff; abandoning all in-flight jobs", "jobs", len(all))
	return DrainOutcome{Running: len(all), FlushTimedOut: true}
}

// enqueueBarrier puts a barrier on the FIFO and waits for the sendLoop to reach
// it, both bounded by the flush deadline (and the stream ctx). Returns false on
// timeout/teardown.
func (c *Client) enqueueBarrier(streamCtx context.Context, outbound chan<- outboundItem, b *barrierState, flushCtx context.Context) bool {
	select {
	case outbound <- outboundItem{barrier: b}:
	case <-flushCtx.Done():
		return false
	case <-streamCtx.Done():
		return false
	}
	select {
	case <-b.reached:
		return true
	case <-flushCtx.Done():
		return false
	case <-streamCtx.Done():
		return false
	}
}

// waitDone waits for every job's Execute goroutine to return (cleanup done),
// bounded by the shared flush deadline.
func (c *Client) waitDone(jobs map[string]*jobState, flushCtx context.Context) {
	for _, st := range jobs {
		select {
		case <-st.done:
		case <-flushCtx.Done():
			return
		}
	}
}
