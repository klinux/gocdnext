package grpcsrv

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gocdnext/gocdnext/server/internal/metrics"
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
	// defaultLogMaxLines bounds the per-stream buffer by line COUNT.
	// 4096 ≈ 13s of headroom at a few hundred lines/s — absorbs the
	// Gradle/testcontainers bursts that dropped at the old 4×batch=400.
	defaultLogMaxLines = 4096
	// defaultLogMaxInflightBytes bounds the buffer by RETAINED BYTES.
	// The pod log scanner cap is 1MB/line (engine.kubernetes*), so a
	// count-only bound of N lines has an N×1MB worst case; the byte cap
	// pins the heap regardless of line size. 16MiB/stream is the
	// pathological ceiling, not the common case (real lines are ~hundreds
	// of bytes).
	defaultLogMaxInflightBytes int64 = 16 << 20 // 16 MiB
	// logMaxLinesCeiling clamps an operator-supplied line cap so a
	// fat-fingered env can't request a near-unbounded channel.
	logMaxLinesCeiling = 65536
	// logMinInflightBytes: one worst-case 1MB line must always fit, else
	// a single fat line would be dropped forever.
	logMinInflightBytes int64 = 1 << 20 // 1 MiB
	// logMaxInflightBytesCeiling caps an operator-supplied byte bound so
	// a fat-fingered GOCDNEXT_LOG_BUFFER_MAX_BYTES (an extra few zeros)
	// can't turn one stream's buffer into an OOM. 512MiB/stream is far
	// above any legitimate tuning need.
	logMaxInflightBytesCeiling int64 = 512 << 20 // 512 MiB
)

// logBatcherConfig carries the per-stream buffer sizing. Env lives in
// config.Load (GOCDNEXT_LOG_BUFFER_MAX_*) and reaches the service via
// WithLogBatcherLimits — NOT read inside the batcher — then is clamped
// so newLogBatcherWithConfig can trust the values.
type logBatcherConfig struct {
	batchSize        int
	flushEvery       time.Duration
	maxLines         int   // channel capacity (line-count bound)
	maxInflightBytes int64 // retained-payload bound: sum of len(LogLine.Text)
}

func defaultLogBatcherConfig() logBatcherConfig {
	return logBatcherConfig{
		batchSize:        defaultLogBatchSize,
		flushEvery:       defaultLogFlushEvery,
		maxLines:         defaultLogMaxLines,
		maxInflightBytes: defaultLogMaxInflightBytes,
	}
}

// clamped enforces the invariants regardless of source (config-derived
// limits OR a WithLogBatcherLimits override):
//   - batchSize > 0, flushEvery > 0
//   - maxLines ∈ [batchSize, logMaxLinesCeiling] (must hold ≥1 batch)
//   - maxInflightBytes ∈ [logMinInflightBytes, logMaxInflightBytesCeiling]
//     (one 1MB line must fit; ceiling caps a fat-fingered env so it can't
//     OOM the process).
func (c logBatcherConfig) clamped() logBatcherConfig {
	if c.batchSize <= 0 {
		c.batchSize = defaultLogBatchSize
	}
	if c.flushEvery <= 0 {
		c.flushEvery = defaultLogFlushEvery
	}
	if c.maxLines < c.batchSize {
		c.maxLines = c.batchSize
	}
	if c.maxLines > logMaxLinesCeiling {
		c.maxLines = logMaxLinesCeiling
	}
	if c.maxInflightBytes < logMinInflightBytes {
		c.maxInflightBytes = logMinInflightBytes
	}
	if c.maxInflightBytes > logMaxInflightBytesCeiling {
		c.maxInflightBytes = logMaxInflightBytesCeiling
	}
	return c
}

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
	// afterFlush, when non-nil, marks this entry as a BARRIER rather
	// than a log line: the flusher drains every line queued before it
	// (preserving recv-loop FIFO order) and only then runs the
	// callback. line/attempt are unused for a barrier. See AfterFlush.
	afterFlush func()
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

	// maxInflightBytes bounds the retained log-text payload (channel +
	// in-flight batch). inflightBytes is the running sum, incremented
	// once when a line is accepted into the buffer (Push) and
	// decremented once when it leaves (any flush outcome). Exactly-once
	// on both edges — a drop never increments; the flush defer always
	// decrements.
	maxInflightBytes int64
	inflightBytes    atomic.Int64

	// Cached drop counters — resolved once so the hot path never pays a
	// WithLabelValues map lookup per drop.
	mDropBackpressure  prometheus.Counter
	mDropBytesFull     prometheus.Counter
	mDropSnapshotStale prometheus.Counter
	mDropFlushFailed   prometheus.Counter
	mSessionDiscarded  prometheus.Counter
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
	return newLogBatcherWithConfig(sink, log, agentID, defaultLogBatcherConfig())
}

// newLogBatcherWithConfig is the real constructor: the channel is sized
// by cfg.maxLines (line-count bound) and the byte bound by
// cfg.maxInflightBytes. cfg is re-clamped defensively so a
// WithLogBatcherLimits override can't smuggle in an invalid shape.
// The drop counters are resolved once here (not per-drop).
func newLogBatcherWithConfig(
	sink logSink,
	log *slog.Logger,
	agentID uuid.UUID,
	cfg logBatcherConfig,
) *logBatcher {
	if log == nil {
		log = slog.Default()
	}
	cfg = cfg.clamped()
	return &logBatcher{
		in:                 make(chan pendingLine, cfg.maxLines),
		done:               make(chan struct{}),
		sink:               sink,
		log:                log,
		batchSize:          cfg.batchSize,
		flushEvery:         cfg.flushEvery,
		agentID:            agentID,
		maxInflightBytes:   cfg.maxInflightBytes,
		mDropBackpressure:  metrics.LogLinesDropped.WithLabelValues("backpressure"),
		mDropBytesFull:     metrics.LogLinesDropped.WithLabelValues("bytes_full"),
		mDropSnapshotStale: metrics.LogLinesDropped.WithLabelValues("snapshot_stale"),
		mDropFlushFailed:   metrics.LogLinesDropped.WithLabelValues("flush_failed"),
		mSessionDiscarded:  metrics.LogBatcherSessionDiscarded,
	}
}

// lineBytes is the retained-payload size the byte bound accounts for:
// the log text itself, which the 1MB scanner cap makes the only field
// that varies by orders of magnitude. Fixed metadata (uuid, seq, ts,
// stream) is intentionally not counted — the bound caps heap under fat
// log lines, not per-line struct overhead.
func lineBytes(l store.LogLine) int64 {
	return int64(len(l.Text))
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
		// Session superseded: this line is never persisted. Count it on
		// the intentional-discard series — a line arriving AFTER Discard
		// is in no batch, so without this it would vanish invisibly (the
		// pending batch is counted at flush; these are not).
		b.mSessionDiscarded.Inc()
		return
	}
	sz := lineBytes(l)
	// Byte-based backpressure, checked BEFORE the channel enqueue: a
	// handful of ~1MB lines can blow the per-stream heap even when the
	// line-count channel still has slots. Single-producer (the stream's
	// recv loop), so this Load-then-Add is race-free against itself; the
	// flusher only ever DECREMENTS, so a concurrent flush can only make
	// inflightBytes smaller — the cap stays a true upper bound.
	if b.inflightBytes.Load()+sz > b.maxInflightBytes {
		b.mDropBytesFull.Inc()
		b.log.Warn("log batcher backpressure, dropping line (bytes cap)",
			"job_run_id", l.JobRunID, "seq", l.Seq)
		return
	}
	entry := pendingLine{line: l, attempt: attempt}
	select {
	case b.in <- entry:
		b.inflightBytes.Add(sz)
	default:
		// Channel full — try once more with a tight deadline before
		// giving up. A 50ms wedge is enough for one in-flight flush
		// to drain a backlog under sustained load.
		select {
		case b.in <- entry:
			b.inflightBytes.Add(sz)
		case <-time.After(50 * time.Millisecond):
			b.mDropBackpressure.Inc()
			b.log.Warn("log batcher backpressure, dropping line",
				"job_run_id", l.JobRunID, "seq", l.Seq)
		}
	}
}

// AfterFlush enqueues a barrier on the same FIFO channel the log
// lines travel: the flusher drains every line pushed before this call
// and only then runs fn. This is how cold-archival is sequenced AFTER
// a terminating job's final lines are durable — enqueueing the archive
// straight from the JobResult handler raced the flush window (200ms)
// and snapshotted log_lines without the trailing markers, which the
// archiver then deleted, eating the job's last lines from the archive.
//
// Because the recv loop pushes a job's log lines (handleLogLine) before
// it handles the JobResult (which calls this), the barrier is always
// behind those lines in the channel — no timing assumption, no grace
// window.
//
// In discard mode (session superseded) the barrier is dropped with the
// pending lines: archiving a reclaimed attempt's truncated tail is
// worse than letting the successor attempt re-archive cleanly. Under
// sustained backpressure the barrier is dropped with a warn rather than
// run inline (running it inline would reintroduce the very race it
// closes); the job keeps its rows in log_lines and a future catch-up
// pass can archive it.
func (b *logBatcher) AfterFlush(fn func()) {
	if b.discard.Load() {
		return
	}
	entry := pendingLine{afterFlush: fn}
	select {
	case b.in <- entry:
	default:
		select {
		case b.in <- entry:
		case <-time.After(50 * time.Millisecond):
			b.log.Warn("log batcher backpressure, dropping archive barrier " +
				"(job keeps log_lines; not archived this pass)")
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
		// Every line in `batch` is leaving the buffer this call —
		// flushed, stale-dropped, DB-failed, or discarded. Release its
		// retained bytes exactly once, on ALL outcomes, via one defer.
		// (Deferred args evaluate now, so this captures the full sum.)
		var batchBytes int64
		for i := range batch {
			batchBytes += lineBytes(batch[i].line)
		}
		defer b.inflightBytes.Add(-batchBytes)

		// Session was superseded mid-batch: drop pending lines on
		// the floor. Inserting them now would either pollute a
		// just-reclaimed row's log_lines OR win the
		// (job_run_id, seq, at) ON CONFLICT race against the new
		// attempt and silently drop ITS legitimate lines. Either
		// way the operator sees a corrupt log tail. Counted on its own
		// series (intentional discard), NEVER on LogLinesDropped.
		if b.discard.Load() {
			b.mSessionDiscarded.Add(float64(len(batch)))
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
					b.mDropSnapshotStale.Add(float64(len(lines)))
					b.log.Warn("log batcher: dropped lines after snapshot stale",
						"job_run_id", k.jobID, "attempt", k.attempt, "lines", len(lines))
					continue
				}
				b.mDropFlushFailed.Add(float64(len(lines)))
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
			if entry.afterFlush != nil {
				// Barrier: persist everything queued before it, then
				// run the callback. flush() already honors discard
				// (drops the batch); mirror that for the callback so a
				// superseded session never archives.
				flush()
				if !b.discard.Load() {
					entry.afterFlush()
				}
				continue
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
