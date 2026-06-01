package grpcsrv

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Defaults sized for the 90% case: a single agent under heavy load
// (`go test -v` on a sizeable module) emits a few hundred lines per
// second. 100/200ms means most flushes fill the batch by size, never
// waiting the full window — which is exactly the latency floor the
// SSE tail trades. 200ms is also the gap a tail-cursor poll would
// catch on its next tick, so a server crash mid-batch loses at most
// one window's worth of lines.
const (
	defaultLogBatchSize  = 100
	defaultLogFlushEvery = 200 * time.Millisecond
)

// logSink is the narrow store interface the batcher actually uses.
// Lifting the dependency to an interface lets unit tests drive the
// batcher with an in-memory recorder — no testcontainer required.
//
// BulkInsertLogLinesForJob carries the snapshot CAS that closes
// the live-agent log-write race: lock the job_run row, verify
// (agent_id, attempt) still matches, then insert. The batcher
// groups its per-flush buffer by (jobID, attempt) and calls this
// per group, passing the attempt the receive-side caller captured
// at Push time. ErrSnapshotStale returned by the store is the
// signal to drop that group — the row was reclaimed/redispatched
// after the receive-side check but before the flush hit the DB.
type logSink interface {
	BulkInsertLogLinesForJob(
		ctx context.Context,
		jobID, expectedAgentID uuid.UUID,
		expectedAttempt int32,
		lines []store.LogLine,
	) error
}

// pendingLine wraps a buffered log line with the (jobID, attempt)
// snapshot the caller observed at receive time. The whole point of
// carrying attempt-per-line (instead of doing a lookup at flush) is
// to keep the tail intact for short-lived jobs: the agent emits a
// few lines, then sends JobResult; CompleteJob succeeds; the result
// handler calls ClearAssignment — but the lines we already captured
// must still flush. If we looked up the assignment at flush time
// instead, ok=false would drop every line of every fast job.
type pendingLine struct {
	line    store.LogLine
	attempt int32
}

// logBatcher buffers per-stream log lines and flushes them in
// batches via the snapshot-CAS log sink. One instance per agent
// stream (Connect spawns one in its lifecycle; nothing global).
//
// Public API is just Push + Stop. Push is non-blocking unless the
// channel buffer is full — and the buffer is sized so under any
// realistic agent throughput the producer never stalls.
type logBatcher struct {
	in   chan pendingLine
	done chan struct{}

	sink       logSink
	log        *slog.Logger
	batchSize  int
	flushEvery time.Duration
	agentID    uuid.UUID

	// discard toggles "drop everything pending, don't accept new
	// lines" mode. Set by Discard() when the owning session has
	// been superseded by a successor Register — pending lines in
	// the buffer pre-date the reclaim that just cleared the row's
	// log_lines, so flushing them would either land stale rows on
	// the new attempt or (worse) win the (job_run_id, seq, at)
	// ON CONFLICT race against the new attempt's lines and silently
	// drop the new ones.
	discard atomic.Bool
}

// newLogBatcher wires a batcher with sensible defaults. The caller
// owns the lifecycle: call Start before pushing, Stop on shutdown.
//
// `agentID` is the session's owning agent — passed to the
// snapshot-CAS log-write so the SQL can validate (agent_id, attempt)
// match before letting the batch land. The attempt is captured
// per-Push (see pendingLine) instead of looked up at flush, so a
// fast-finishing job whose JobResult triggers ClearAssignment
// before the next flush still has its tail persisted.
func newLogBatcher(
	sink logSink,
	log *slog.Logger,
	agentID uuid.UUID,
) *logBatcher {
	if log == nil {
		log = slog.Default()
	}
	return &logBatcher{
		// Buffer 4× the batch size: lets a burst of agent emissions
		// queue up while a flush is in flight without forcing the
		// caller to block on Push. 4×100 = 400 lines ~ a few hundred
		// KB worst case — bounded.
		in:         make(chan pendingLine, 4*defaultLogBatchSize),
		done:       make(chan struct{}),
		sink:       sink,
		log:        log,
		batchSize:  defaultLogBatchSize,
		flushEvery: defaultLogFlushEvery,
		agentID:    agentID,
	}
}

// Start launches the flusher goroutine. The goroutine exits when
// either ctx is cancelled or the input channel is closed (Stop).
func (b *logBatcher) Start(ctx context.Context) {
	go b.run(ctx)
}

// Push enqueues a line for batched insertion, tagged with the
// (jobID, attempt) snapshot the caller validated at receive time.
// Blocks only if the buffer is full; falls through to a slog warn
// + drop after a tiny timeout so a stuck DB can't pin the gRPC recv
// goroutine forever.
//
// The drop is observable (warn-level log + future metric); operators
// see backpressure rather than silent latency.
//
// In discard mode (set by Discard() after the session was superseded)
// Push returns without queuing the line. The caller's recv-loop
// upstream is already gating on sess.revoked, but lines may have
// been mid-flight here when revoke fired.
func (b *logBatcher) Push(l store.LogLine, attempt int32) {
	if b.discard.Load() {
		return
	}
	entry := pendingLine{line: l, attempt: attempt}
	select {
	case b.in <- entry:
	default:
		// Channel full — try once more with a tight deadline before
		// giving up. A 50ms wedge is enough for one in-flight flush
		// to drain a backlog under sustained load.
		select {
		case b.in <- entry:
		case <-time.After(50 * time.Millisecond):
			b.log.Warn("log batcher backpressure, dropping line",
				"job_run_id", l.JobRunID, "seq", l.Seq)
		}
	}
}

// Discard flips the batcher into "drop pending, refuse new" mode.
// Called from the Connect handler defer when the session was
// superseded by a successor Register. The run loop's next flush
// (either the ticker fire or the Stop drain) sees the flag and
// throws away whatever's in `batch`.
//
// Idempotent — multiple calls are safe.
func (b *logBatcher) Discard() {
	b.discard.Store(true)
}

// Stop signals the flusher to drain anything pending and exit.
// Returns once the goroutine has finished — safe to call from a
// defer in the Connect handler.
func (b *logBatcher) Stop() {
	close(b.in)
	<-b.done
}

// flushKey groups buffered lines by the snapshot-CAS dimensions:
// the row identity (jobID) and the attempt the receive-side
// observed at Push time. Two pushes against the same jobID with
// different attempts (cross-rerun racing or testing) MUST land in
// separate sink calls so the per-attempt CAS rejects exactly the
// one whose snapshot is stale, not both.
type flushKey struct {
	jobID   uuid.UUID
	attempt int32
}

// run is the flusher goroutine body. Two flush triggers: the
// channel filling to batchSize, or the ticker firing. ctx
// cancellation also triggers a final drain so a server shutdown
// doesn't lose a window.
func (b *logBatcher) run(ctx context.Context) {
	defer close(b.done)
	ticker := time.NewTicker(b.flushEvery)
	defer ticker.Stop()

	batch := make([]pendingLine, 0, b.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Session was superseded mid-batch: drop pending lines on
		// the floor. Inserting them now would either pollute a
		// just-reclaimed row's log_lines OR win the
		// (job_run_id, seq, at) ON CONFLICT race against the new
		// attempt and silently drop ITS legitimate lines. Either
		// way the operator sees a corrupt log tail.
		if b.discard.Load() {
			batch = batch[:0]
			return
		}
		// Group by (jobID, attempt) so each insert lands in one
		// snapshot-CAS transaction per (job, attempt) pair. A batch
		// from one agent stream typically covers a handful of
		// concurrently-running jobs (== capacity), so the cardinality
		// is low — and pushing a per-line attempt instead of a
		// shared one means a single batch can carry both the tail of
		// a just-completed attempt and the head of a redispatched
		// one without conflating their CAS predicates.
		groups := make(map[flushKey][]store.LogLine, 4)
		for _, entry := range batch {
			k := flushKey{jobID: entry.line.JobRunID, attempt: entry.attempt}
			groups[k] = append(groups[k], entry.line)
		}
		// Use a separate context for the flush itself so ctx-
		// cancellation during shutdown still attempts a best-effort
		// write (with a short timeout) rather than dropping the
		// pending batch.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		for k, lines := range groups {
			err := b.sink.BulkInsertLogLinesForJob(flushCtx, k.jobID, b.agentID, k.attempt, lines)
			if err != nil {
				if errors.Is(err, store.ErrSnapshotStale) {
					// Concurrent reaper/fence/rerun moved the row's
					// (agent, attempt) since the receive-side
					// captured this group's attempt. Drop — the next
					// attempt owns the row now.
					b.log.Warn("log batcher: dropped lines after snapshot stale",
						"job_run_id", k.jobID, "attempt", k.attempt, "lines", len(lines))
					continue
				}
				b.log.Warn("log batcher flush failed",
					"err", err, "job_run_id", k.jobID, "attempt", k.attempt, "lines", len(lines))
			}
		}
		cancel()
		// Reset slice while keeping capacity — avoids reallocation
		// on every flush.
		batch = batch[:0]
	}

	for {
		select {
		case entry, ok := <-b.in:
			if !ok {
				flush()
				return
			}
			batch = append(batch, entry)
			if len(batch) >= b.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			flush()
			return
		}
	}
}
