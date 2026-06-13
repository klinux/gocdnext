package grpcsrv

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// recordingSink captures every batch the flusher submits along with
// the (jobID, expectedAgentID, expectedAttempt) tuple the caller
// asserts via the snapshot-CAS sink contract. Tests can reason
// about boundaries (size-based vs time-based, drain on Stop) AND
// the per-(job, attempt) grouping without booting Postgres.
type recordingSink struct {
	mu      sync.Mutex
	batches []recordedBatch
	failOn  func(batch recordedBatch) error
}

type recordedBatch struct {
	JobID   uuid.UUID
	AgentID uuid.UUID
	Attempt int32
	Lines   []store.LogLine
}

func (r *recordingSink) BulkInsertLogLinesForJob(
	_ context.Context,
	jobID, expectedAgentID uuid.UUID,
	expectedAttempt int32,
	lines []store.LogLine,
) error {
	cp := make([]store.LogLine, len(lines))
	copy(cp, lines)
	rec := recordedBatch{
		JobID:   jobID,
		AgentID: expectedAgentID,
		Attempt: expectedAttempt,
		Lines:   cp,
	}
	if r.failOn != nil {
		if err := r.failOn(rec); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, rec)
	return nil
}

func (r *recordingSink) snapshot() []recordedBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedBatch, len(r.batches))
	copy(out, r.batches)
	return out
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// makeLine returns a log line scoped to a single fixed job_run_id
// (sharedJobID) so batched-then-flushed tests see ONE group per
// flush. The per-(job, attempt) grouping the batcher now does for
// snapshot CAS means tests that want to assert "one batch of N
// lines" must keep the (jobID, attempt) tuple stable across pushes.
func makeLine(seq int64) store.LogLine {
	return store.LogLine{
		JobRunID: sharedJobID,
		Seq:      seq,
		Stream:   "stdout",
		At:       time.Now().UTC(),
		Text:     "x",
	}
}

// sharedJobID is the stable job_run_id every makeLine sample uses.
// Pinned at package-init time so the address-sanitizer's run order
// doesn't reshuffle it across tests within the same process.
var sharedJobID = uuid.New()

// TestLogBatcher_FlushesOnSize pins the size-based flush trigger.
// At batchSize=100, pushing exactly 100 lines must produce one
// batch with 100 lines BEFORE the ticker would have fired.
func TestLogBatcher_FlushesOnSize(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger(), uuid.New())
	b.flushEvery = 10 * time.Second // tick should not contribute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	for i := int64(0); i < int64(b.batchSize); i++ {
		b.Push(makeLine(i), 1)
	}
	// Give the goroutine a chance to flush — orders of magnitude
	// shorter than flushEvery, so a missed flush will be obvious.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(sink.snapshot()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.Stop()

	got := sink.snapshot()
	if len(got) == 0 {
		t.Fatalf("expected size-triggered flush, got no batches")
	}
	if n := len(got[0].Lines); n != b.batchSize {
		t.Errorf("first batch size = %d, want %d", n, b.batchSize)
	}
}

// TestLogBatcher_FlushesOnTimer pins the time-based flush — pushing
// fewer than batchSize lines must still produce a flush after
// flushEvery.
func TestLogBatcher_FlushesOnTimer(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger(), uuid.New())
	b.flushEvery = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	for i := int64(0); i < 5; i++ {
		b.Push(makeLine(i), 1)
	}

	// Wait for at least one tick to fire.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(sink.snapshot()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.Stop()

	got := sink.snapshot()
	if len(got) == 0 {
		t.Fatalf("expected timer flush, got none")
	}
	total := 0
	for _, batch := range got {
		total += len(batch.Lines)
	}
	if total != 5 {
		t.Errorf("total lines flushed = %d, want 5", total)
	}
}

// TestLogBatcher_DiscardDropsPendingOnStop locks in the
// superseded-session contract: when the owning Connect handler
// observes its session was superseded by a successor Register, it
// calls Discard() before Stop(). The drain on Stop must skip the
// DB write so stale lines from the old stream don't land on a row
// the reclaim just cleared (or worse, win the (job_run_id, seq, at)
// ON CONFLICT race against the new attempt's legitimate lines and
// silently drop those).
func TestLogBatcher_DiscardDropsPendingOnStop(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger(), uuid.New())
	b.flushEvery = 10 * time.Second // never fires during the test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	// Push some lines that would normally drain on Stop.
	for i := int64(0); i < 5; i++ {
		b.Push(makeLine(i), 1)
	}
	// Superseded: flip the flag BEFORE Stop, mirroring the
	// Connect handler's defer order.
	b.Discard()
	b.Stop()

	got := sink.snapshot()
	total := 0
	for _, batch := range got {
		total += len(batch.Lines)
	}
	if total != 0 {
		t.Errorf("Discard+Stop wrote %d lines to sink, want 0 (stale-write would corrupt next attempt)", total)
	}
}

// TestLogBatcher_DiscardRejectsNewPushes — Push after Discard must
// be a no-op so the Connect handler's revoke check + Discard
// together fully gate the path. (Even though the recv loop also
// skips revoked sessions before Push, races are inevitable; making
// Push idempotent under Discard is the belt to the recv-loop's
// suspenders.)
func TestLogBatcher_DiscardRejectsNewPushes(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger(), uuid.New())
	b.flushEvery = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	b.Discard()
	for i := int64(0); i < 3; i++ {
		b.Push(makeLine(i), 1)
	}
	b.Stop()

	got := sink.snapshot()
	total := 0
	for _, batch := range got {
		total += len(batch.Lines)
	}
	if total != 0 {
		t.Errorf("Push after Discard wrote %d lines, want 0", total)
	}
}

// TestLogBatcher_FlushAfterAssignmentClearedKeepsTail is THE
// regression test for the fast-job-tail-loss bug. The earlier
// implementation looked up the session's assignment at flush time;
// when a fast job emitted a few log lines and then JobResult, the
// result handler called ClearAssignment, the next ticker fired, and
// the lookup returned ok=false — dropping the entire tail. The fix:
// the receive-side captures the attempt at Push time and tags each
// line with it. Even if ClearAssignment fires between push and
// flush, the captured attempt is what the batcher uses, so the
// snapshot CAS lands at the DB layer (where the row's actual
// (agent_id, attempt) is still intact for a healthy completion).
func TestLogBatcher_FlushAfterAssignmentClearedKeepsTail(t *testing.T) {
	sink := &recordingSink{}
	agentID := uuid.New()
	b := newLogBatcher(sink, silentLogger(), agentID)
	b.flushEvery = 10 * time.Second // only Stop drains
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	// Simulate a fast-job tail: 3 lines pushed with the captured
	// attempt (snapshot the handler observed at receive time was 1).
	for i := int64(0); i < 3; i++ {
		b.Push(makeLine(i), 1)
	}
	// JobResult arrives, ClearAssignment runs — that's external to
	// the batcher. The batcher has already buffered with attempt=1.
	b.Stop()

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 sink call (the captured-attempt batch), got %d", len(got))
	}
	if len(got[0].Lines) != 3 {
		t.Errorf("tail lines persisted = %d, want 3", len(got[0].Lines))
	}
	if got[0].Attempt != 1 {
		t.Errorf("flushed attempt = %d, want 1 (captured at Push time)", got[0].Attempt)
	}
	if got[0].AgentID != agentID {
		t.Errorf("flushed agentID = %v, want %v", got[0].AgentID, agentID)
	}
}

// TestLogBatcher_GroupsByJobOnFlush — a batch covering multiple
// distinct jobs results in ONE sink call per job. The snapshot-CAS
// sink interface is per-(job, attempt), so flush must group before
// the store hit.
func TestLogBatcher_GroupsByJobOnFlush(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger(), uuid.New())
	b.flushEvery = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	jobA := uuid.New()
	jobB := uuid.New()
	b.Push(store.LogLine{JobRunID: jobA, Seq: 1, Stream: "stdout", Text: "a1", At: time.Now()}, 1)
	b.Push(store.LogLine{JobRunID: jobB, Seq: 1, Stream: "stdout", Text: "b1", At: time.Now()}, 1)
	b.Push(store.LogLine{JobRunID: jobA, Seq: 2, Stream: "stdout", Text: "a2", At: time.Now()}, 1)
	b.Stop()

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 sink calls (one per job), got %d", len(got))
	}
	// Each batch carries the lines for one job — assert no
	// cross-job mixing.
	for _, batch := range got {
		seen := batch.Lines[0].JobRunID
		for _, l := range batch.Lines {
			if l.JobRunID != seen {
				t.Errorf("batch mixed jobs: %v vs %v", seen, l.JobRunID)
			}
			if l.JobRunID != batch.JobID {
				t.Errorf("line jobID %v doesn't match batch JobID %v", l.JobRunID, batch.JobID)
			}
		}
	}
}

// TestLogBatcher_GroupsByJobAndAttempt — two pushes against the
// SAME jobID with DIFFERENT attempts must land in separate sink
// calls so the per-attempt snapshot CAS rejects exactly the stale
// one, not both. The scenario this guards: a long-lived stream's
// in-flight buffer holds tail lines from attempt N when a register-
// fence requeues the row (attempt → N+1) and the agent's recv loop
// — still alive on the old session — somehow ships a line for the
// new attempt before its revoke check fires. Conflating both into
// one sink call would force one shared (jobID, attempt) tuple and
// either drop everything (CAS fails) or persist the stale tail
// under N+1's attempt counter (CAS passes for the wrong rows).
func TestLogBatcher_GroupsByJobAndAttempt(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger(), uuid.New())
	b.flushEvery = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	jobID := uuid.New()
	b.Push(store.LogLine{JobRunID: jobID, Seq: 1, Stream: "stdout", Text: "a", At: time.Now()}, 1)
	b.Push(store.LogLine{JobRunID: jobID, Seq: 2, Stream: "stdout", Text: "b", At: time.Now()}, 2)
	b.Push(store.LogLine{JobRunID: jobID, Seq: 3, Stream: "stdout", Text: "c", At: time.Now()}, 1)
	b.Stop()

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 sink calls (one per attempt), got %d", len(got))
	}
	byAttempt := map[int32]int{}
	for _, batch := range got {
		byAttempt[batch.Attempt] += len(batch.Lines)
	}
	if byAttempt[1] != 2 {
		t.Errorf("attempt=1 lines = %d, want 2", byAttempt[1])
	}
	if byAttempt[2] != 1 {
		t.Errorf("attempt=2 lines = %d, want 1", byAttempt[2])
	}
}

// TestLogBatcher_DrainOnStop guarantees Stop flushes anything left
// in the buffer — operators rely on this so a clean shutdown
// doesn't drop the last partial window.
func TestLogBatcher_DrainOnStop(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger(), uuid.New())
	b.flushEvery = 10 * time.Second // ticker won't fire
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	for i := int64(0); i < 7; i++ {
		b.Push(makeLine(i), 1)
	}
	b.Stop() // must flush despite size < batchSize and ticker not yet

	got := sink.snapshot()
	total := 0
	for _, batch := range got {
		total += len(batch.Lines)
	}
	if total != 7 {
		t.Errorf("after Stop total = %d, want 7", total)
	}
}

// TestLogBatcher_AfterFlushRunsAfterPriorLinesPersisted is the
// regression for the cold-archive race: archival used to be enqueued
// straight from the JobResult handler, which fired BEFORE the 200ms
// batcher flush had written the job's final lines. The archiver then
// snapshotted log_lines without them, deleted the rows, and the
// trailing markers were lost from the archive (operator-reported
// "post-job …" with no "done" and the cache-store eaten).
//
// The barrier travels the same FIFO channel as the lines, so by the
// time the flusher reaches it every prior line is already in the
// sink. The assertion captures the sink count AT THE MOMENT the
// barrier runs — not after — so a flush-after-barrier ordering bug
// would fail here.
func TestLogBatcher_AfterFlushRunsAfterPriorLinesPersisted(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger(), uuid.New())
	b.flushEvery = 10 * time.Second // force the barrier (not the ticker) to drive the flush
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	const n = 7
	for i := int64(0); i < n; i++ {
		b.Push(makeLine(i), 1)
	}

	seenAtBarrier := make(chan int, 1)
	b.AfterFlush(func() {
		total := 0
		for _, batch := range sink.snapshot() {
			total += len(batch.Lines)
		}
		seenAtBarrier <- total
	})

	select {
	case got := <-seenAtBarrier:
		if got != n {
			t.Fatalf("barrier saw %d persisted lines, want %d — flush did not precede the barrier", got, n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("barrier never ran")
	}
	b.Stop()
}

// TestLogBatcher_AfterFlushSkippedInDiscard pins that a superseded
// session (Discard) drops the archive barrier with the pending
// lines — archiving a reclaimed attempt's truncated tail would be
// worse than letting the successor attempt re-archive cleanly.
func TestLogBatcher_AfterFlushSkippedInDiscard(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger(), uuid.New())
	b.flushEvery = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	b.Discard()
	ran := make(chan struct{}, 1)
	b.AfterFlush(func() { ran <- struct{}{} })

	select {
	case <-ran:
		t.Fatal("barrier ran despite discard — would archive a superseded attempt")
	case <-time.After(200 * time.Millisecond):
		// expected: barrier dropped
	}
	b.Stop()
}
