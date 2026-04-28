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

// recordingSink captures every batch the flusher submits so tests
// can reason about boundaries (size-based vs time-based flushes,
// drain on Stop, etc.) without booting Postgres.
type recordingSink struct {
	mu      sync.Mutex
	batches [][]store.LogLine
	failOn  func(batch []store.LogLine) error
}

func (r *recordingSink) BulkInsertLogLines(_ context.Context, lines []store.LogLine) error {
	if r.failOn != nil {
		if err := r.failOn(lines); err != nil {
			return err
		}
	}
	cp := make([]store.LogLine, len(lines))
	copy(cp, lines)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, cp)
	return nil
}

func (r *recordingSink) snapshot() [][]store.LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]store.LogLine, len(r.batches))
	copy(out, r.batches)
	return out
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeLine(seq int64) store.LogLine {
	return store.LogLine{
		JobRunID: uuid.New(),
		Seq:      seq,
		Stream:   "stdout",
		At:       time.Now().UTC(),
		Text:     "x",
	}
}

// TestLogBatcher_FlushesOnSize pins the size-based flush trigger.
// At batchSize=100, pushing exactly 100 lines must produce one
// batch with 100 lines BEFORE the ticker would have fired.
func TestLogBatcher_FlushesOnSize(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger())
	b.flushEvery = 10 * time.Second // tick should not contribute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	for i := int64(0); i < int64(b.batchSize); i++ {
		b.Push(makeLine(i))
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
	if n := len(got[0]); n != b.batchSize {
		t.Errorf("first batch size = %d, want %d", n, b.batchSize)
	}
}

// TestLogBatcher_FlushesOnTimer pins the time-based flush — pushing
// fewer than batchSize lines must still produce a flush after
// flushEvery.
func TestLogBatcher_FlushesOnTimer(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger())
	b.flushEvery = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	for i := int64(0); i < 5; i++ {
		b.Push(makeLine(i))
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
		total += len(batch)
	}
	if total != 5 {
		t.Errorf("total lines flushed = %d, want 5", total)
	}
}

// TestLogBatcher_DrainOnStop guarantees Stop flushes anything left
// in the buffer — operators rely on this so a clean shutdown
// doesn't drop the last partial window.
func TestLogBatcher_DrainOnStop(t *testing.T) {
	sink := &recordingSink{}
	b := newLogBatcher(sink, silentLogger())
	b.flushEvery = 10 * time.Second // ticker won't fire
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	for i := int64(0); i < 7; i++ {
		b.Push(makeLine(i))
	}
	b.Stop() // must flush despite size < batchSize and ticker not yet

	got := sink.snapshot()
	total := 0
	for _, batch := range got {
		total += len(batch)
	}
	if total != 7 {
		t.Errorf("after Stop total = %d, want 7", total)
	}
}
