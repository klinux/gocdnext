package grpcsrv

import (
	"context"
	"log/slog"
	"time"

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
type logSink interface {
	BulkInsertLogLines(ctx context.Context, lines []store.LogLine) error
}

// logBatcher buffers per-stream log lines and flushes them in
// batches via BulkInsertLogLines. One instance per agent stream
// (Connect spawns one in its lifecycle; nothing global).
//
// Public API is just Push + Stop. Push is non-blocking unless the
// channel buffer is full — and the buffer is sized so under any
// realistic agent throughput the producer never stalls.
type logBatcher struct {
	in   chan store.LogLine
	done chan struct{}

	sink       logSink
	log        *slog.Logger
	batchSize  int
	flushEvery time.Duration
}

// newLogBatcher wires a batcher with sensible defaults. The caller
// owns the lifecycle: call Start before pushing, Stop on shutdown.
func newLogBatcher(sink logSink, log *slog.Logger) *logBatcher {
	if log == nil {
		log = slog.Default()
	}
	return &logBatcher{
		// Buffer 4× the batch size: lets a burst of agent emissions
		// queue up while a flush is in flight without forcing the
		// caller to block on Push. 4×100 = 400 lines ~ a few hundred
		// KB worst case — bounded.
		in:         make(chan store.LogLine, 4*defaultLogBatchSize),
		done:       make(chan struct{}),
		sink:       sink,
		log:        log,
		batchSize:  defaultLogBatchSize,
		flushEvery: defaultLogFlushEvery,
	}
}

// Start launches the flusher goroutine. The goroutine exits when
// either ctx is cancelled or the input channel is closed (Stop).
func (b *logBatcher) Start(ctx context.Context) {
	go b.run(ctx)
}

// Push enqueues a line for batched insertion. Blocks only if the
// buffer is full; falls through to a slog warn + drop after a tiny
// timeout so a stuck DB can't pin the gRPC recv goroutine forever.
//
// The drop is observable (warn-level log + future metric); operators
// see backpressure rather than silent latency.
func (b *logBatcher) Push(l store.LogLine) {
	select {
	case b.in <- l:
	default:
		// Channel full — try once more with a tight deadline before
		// giving up. A 50ms wedge is enough for one in-flight flush
		// to drain a backlog under sustained load.
		select {
		case b.in <- l:
		case <-time.After(50 * time.Millisecond):
			b.log.Warn("log batcher backpressure, dropping line",
				"job_run_id", l.JobRunID, "seq", l.Seq)
		}
	}
}

// Stop signals the flusher to drain anything pending and exit.
// Returns once the goroutine has finished — safe to call from a
// defer in the Connect handler.
func (b *logBatcher) Stop() {
	close(b.in)
	<-b.done
}

// run is the flusher goroutine body. Two flush triggers: the
// channel filling to batchSize, or the ticker firing. ctx
// cancellation also triggers a final drain so a server shutdown
// doesn't lose a window.
func (b *logBatcher) run(ctx context.Context) {
	defer close(b.done)
	ticker := time.NewTicker(b.flushEvery)
	defer ticker.Stop()

	batch := make([]store.LogLine, 0, b.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Use a separate context for the flush itself so ctx-
		// cancellation during shutdown still attempts a best-effort
		// write (with a short timeout) rather than dropping the
		// pending batch.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := b.sink.BulkInsertLogLines(flushCtx, batch)
		cancel()
		if err != nil {
			b.log.Warn("log batcher flush failed",
				"err", err, "lines", len(batch))
		}
		// Reset slice while keeping capacity — avoids reallocation
		// on every flush.
		batch = batch[:0]
	}

	for {
		select {
		case line, ok := <-b.in:
			if !ok {
				flush()
				return
			}
			batch = append(batch, line)
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
