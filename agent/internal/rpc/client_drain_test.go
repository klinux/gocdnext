package rpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// These tests drive the drain protocol (sendLoop barrier handling + the drain
// methods) directly over a fake stream, so the two-barrier rendezvous,
// per-identity suppression and outcome contracts are exercised deterministically
// — no real runner, no timing races. They are package-internal (package rpc) so
// they can reach the unexported outbound envelope + jobState.

// fakeClientStream records Send()s and, on CloseSend, simulates the server
// processing everything and half-closing (client sees io.EOF on Recv) by pushing
// io.EOF onto recvErrCh — the drain's "server processed" confirmation.
type fakeClientStream struct {
	gocdnextv1.AgentService_ConnectClient // embedded (nil): only Send/Recv/CloseSend/Context are called
	ctx                                   context.Context
	mu                                    sync.Mutex
	sent                                  []*gocdnextv1.AgentMessage
	closed                                bool
	recvErrCh                             chan<- error
}

func (s *fakeClientStream) Context() context.Context { return s.ctx }

func (s *fakeClientStream) Send(m *gocdnextv1.AgentMessage) error {
	s.mu.Lock()
	s.sent = append(s.sent, m)
	s.mu.Unlock()
	return nil
}

func (s *fakeClientStream) CloseSend() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if s.recvErrCh != nil {
		select {
		case s.recvErrCh <- io.EOF:
		default:
		}
	}
	return nil
}

// jobOutputsSent returns the job_ids of the OUTPUT messages actually delivered.
func (s *fakeClientStream) jobOutputsSent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, m := range s.sent {
		if id, ok := jobOutputID(m); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func drainTestClient() *Client {
	return New(Config{FlushTimeout: 2 * time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func jobResultMsg(jobID string) *gocdnextv1.AgentMessage {
	return &gocdnextv1.AgentMessage{Kind: &gocdnextv1.AgentMessage_Result{
		Result: &gocdnextv1.JobResult{JobId: jobID},
	}}
}

func coverageMsg(jobID string) *gocdnextv1.AgentMessage {
	return &gocdnextv1.AgentMessage{Kind: &gocdnextv1.AgentMessage_Coverage{
		Coverage: &gocdnextv1.CoverageSummary{JobId: jobID},
	}}
}

// TestDrain_CleanNoJobs: zero in-flight → flushClean → CloseSend → EOF → Clean.
func TestDrain_CleanNoJobs(t *testing.T) {
	c := drainTestClient()
	c.cfg.DrainBudget = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbound := make(chan outboundItem, 16)
	recvErrCh := make(chan error, 1)
	fs := &fakeClientStream{ctx: ctx, recvErrCh: recvErrCh}
	go func() { _ = c.sendLoop(ctx, fs, time.Hour, outbound) }()

	outcome := c.drain(ctx, outbound, recvErrCh)
	if !outcome.Clean || outcome.Running != 0 || outcome.FlushTimedOut {
		t.Fatalf("clean drain outcome = %+v, want {Clean:true}", outcome)
	}
	fs.mu.Lock()
	closed := fs.closed
	fs.mu.Unlock()
	if !closed {
		t.Fatal("clean drain did not CloseSend")
	}
}

// TestDrain_TimeoutPreservesTerminalSent: a job whose JobResult crossed the FIFO
// before the cutoff is preserved (terminalSent) → not suppressed → Running=0.
func TestDrain_TimeoutPreservesTerminalSent(t *testing.T) {
	c := drainTestClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbound := make(chan outboundItem, 16)
	recvErrCh := make(chan error, 1)
	fs := &fakeClientStream{ctx: ctx, recvErrCh: recvErrCh}
	go func() { _ = c.sendLoop(ctx, fs, time.Hour, outbound) }()

	// Job A: its result already crossed; Execute has finished (done closed).
	aDone := make(chan struct{})
	stA := &jobState{cancel: func() {}, done: aDone}
	c.jobsMu.Lock()
	c.running["A"] = stA
	c.jobsMu.Unlock()

	// Deliver A's JobResult through the real sendLoop → flips terminalSent.
	outbound <- outboundItem{msg: jobResultMsg("A"), job: stA}
	waitFor(t, func() bool { return stA.terminalSent.Load() }, "A terminalSent")
	close(aDone) // A's Execute returned (cleanup done)

	// DrainBudget<=0 forces the timeout path directly.
	outcome := c.drainTimeout(ctx, outbound, recvErrCh)
	if outcome.Running != 0 {
		t.Fatalf("Running = %d, want 0 (A had terminalSent, must be preserved)", outcome.Running)
	}
	if got := fs.jobOutputsSent(); len(got) != 1 || got[0] != "A" {
		t.Fatalf("delivered outputs = %v, want [A]", got)
	}
}

// TestDrain_TimeoutSuppressesRunningJob: a job with no terminalSent at the cutoff
// is aborted (Client-owned cancel), counted, and its LATER output is dropped.
func TestDrain_TimeoutSuppressesRunningJob(t *testing.T) {
	c := drainTestClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbound := make(chan outboundItem, 16)
	recvErrCh := make(chan error, 1)
	fs := &fakeClientStream{ctx: ctx, recvErrCh: recvErrCh}
	go func() { _ = c.sendLoop(ctx, fs, time.Hour, outbound) }()

	var cancelled bool
	bDone := make(chan struct{})
	stB := &jobState{cancel: func() { cancelled = true; close(bDone) }, done: bDone}
	c.jobsMu.Lock()
	c.running["B"] = stB
	c.jobsMu.Unlock()

	outcome := c.drainTimeout(ctx, outbound, recvErrCh)
	if outcome.Running != 1 {
		t.Fatalf("Running = %d, want 1 (B had no terminalSent)", outcome.Running)
	}
	if !cancelled {
		t.Fatal("B was not aborted via the Client-owned cancel")
	}
	// B's post-cutoff output would now be dropped at the dequeue check.
	if s := c.suppressed.Load(); s == nil {
		t.Fatal("suppression set not published")
	} else if _, ok := (*s)["B"]; !ok {
		t.Fatal("B not in the suppression set")
	}
}

// TestSendLoop_DropsSuppressedOutput: with a suppression set published, the
// sendLoop drops a suppressed job's buffered output at dequeue.
func TestSendLoop_DropsSuppressedOutput(t *testing.T) {
	c := drainTestClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbound := make(chan outboundItem, 16)
	fs := &fakeClientStream{ctx: ctx}
	go func() { _ = c.sendLoop(ctx, fs, time.Hour, outbound) }()

	sup := map[string]struct{}{"B": {}}
	c.suppressed.Store(&sup)

	outbound <- outboundItem{msg: coverageMsg("A")} // not suppressed → delivered
	outbound <- outboundItem{msg: coverageMsg("B")} // suppressed → dropped
	waitFor(t, func() bool {
		got := fs.jobOutputsSent()
		return len(got) == 1 && got[0] == "A"
	}, "only A's coverage delivered")
	if got := fs.jobOutputsSent(); len(got) != 1 || got[0] != "A" {
		t.Fatalf("delivered = %v, want [A] (B suppressed)", got)
	}
}

// TestDrain_FailSafeSuppressesAllWhenCutoffUnreached: a wedged sendLoop (never
// consumes the cutoff) → flush deadline hits → suppress-all + FlushTimedOut with
// the abandoned count, no output leaks.
func TestDrain_FailSafeSuppressesAllWhenCutoffUnreached(t *testing.T) {
	c := New(Config{FlushTimeout: 100 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// NOTE: no sendLoop started → the cutoff barrier is never reached.
	outbound := make(chan outboundItem, 1)
	recvErrCh := make(chan error, 1)

	var b1c, b2c bool
	c.jobsMu.Lock()
	c.running["B1"] = &jobState{cancel: func() { b1c = true }, done: make(chan struct{})}
	c.running["B2"] = &jobState{cancel: func() { b2c = true }, done: make(chan struct{})}
	c.jobsMu.Unlock()

	outcome := c.drainTimeout(ctx, outbound, recvErrCh)
	if !outcome.FlushTimedOut || outcome.Running != 2 {
		t.Fatalf("fail-safe outcome = %+v, want {Running:2, FlushTimedOut:true}", outcome)
	}
	if !b1c || !b2c {
		t.Fatal("fail-safe did not cancel every uncertain job")
	}
	// suppress-all published BEFORE any release, so both are suppressed.
	s := c.suppressed.Load()
	if s == nil || len((*s)) != 2 {
		t.Fatalf("suppress-all set = %v, want {B1,B2}", s)
	}
}

// TestDrain_CleanFlushNonEOFIsNotClean: a NON-EOF recv end (transport loss) must
// NOT be reported as a clean drain — EOF is the only proof the server processed
// every JobResult. (Regression for review finding 1.)
func TestDrain_CleanFlushNonEOFIsNotClean(t *testing.T) {
	c := New(Config{FlushTimeout: 500 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbound := make(chan outboundItem, 16)
	// The drain reads this channel; the fake stream's own recvErrCh is left nil so
	// CloseSend pushes nothing — the recv end here is the transport error we inject.
	recvErrCh := make(chan error, 1)
	recvErrCh <- errors.New("transport closed mid-flush")
	fs := &fakeClientStream{ctx: ctx}
	go func() { _ = c.sendLoop(ctx, fs, time.Hour, outbound) }()

	outcome := c.flushClean(ctx, outbound, recvErrCh)
	if outcome.Clean {
		t.Fatalf("outcome.Clean = true on a non-EOF recv error; want not clean: %+v", outcome)
	}
	if !outcome.FlushTimedOut {
		t.Fatalf("outcome = %+v, want FlushTimedOut=true (abandon to reaper)", outcome)
	}
}

// TestDrain_TimeoutFinalUnconfirmedAbandonsAll: after a normal cutoff+suppression,
// if the final flush is never confirmed (no server EOF), the PRESERVED survivor
// (terminalSent) is also unconfirmed and must be abandoned — Running counts every
// uncertain candidate, not just the suppressed set. (Regression for finding 2.)
func TestDrain_TimeoutFinalUnconfirmedAbandonsAll(t *testing.T) {
	c := New(Config{FlushTimeout: 200 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbound := make(chan outboundItem, 16)
	recvErrCh := make(chan error, 1)  // never receives → the final EOF wait times out
	fs := &fakeClientStream{ctx: ctx} // recvErrCh nil → CloseSend pushes no EOF
	go func() { _ = c.sendLoop(ctx, fs, time.Hour, outbound) }()

	// A: result already crossed (terminalSent), Execute finished.
	aDone := make(chan struct{})
	close(aDone)
	stA := &jobState{cancel: func() {}, done: aDone}
	// B: still running, no terminalSent → suppressed at the cutoff. Its cancel is
	// idempotent (sync.Once), mirroring a real context.CancelFunc — abandonAll
	// re-cancels every job on the fail-safe path.
	bDone := make(chan struct{})
	var bOnce sync.Once
	stB := &jobState{cancel: func() { bOnce.Do(func() { close(bDone) }) }, done: bDone}
	c.jobsMu.Lock()
	c.running["A"] = stA
	c.running["B"] = stB
	c.jobsMu.Unlock()

	outbound <- outboundItem{msg: jobResultMsg("A"), job: stA}
	waitFor(t, func() bool { return stA.terminalSent.Load() }, "A terminalSent")

	outcome := c.drainTimeout(ctx, outbound, recvErrCh)
	if !outcome.FlushTimedOut || outcome.Running != 2 {
		t.Fatalf("outcome = %+v, want {Running:2, FlushTimedOut:true} (survivor A unconfirmed → abandon all)", outcome)
	}
}

// waitFor polls cond up to 2s.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
