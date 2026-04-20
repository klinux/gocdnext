// Package retention runs background processes that reclaim storage
// space from artefacts that have passed their retention policy.
//
// The sweeper ticks on a schedule, claims a bounded batch of artefacts
// the DB considers expired, deletes each blob from the configured
// storage backend, and then removes the DB row. It's safe to run
// multiple instances concurrently — `FOR UPDATE SKIP LOCKED` in the
// claim query partitions work.
//
// Layers implemented in this slice:
//   - TTL: rows with expires_at in the past.
//   - Retry: rows stuck in 'deleting' for longer than the grace
//     window (sweeper crashed between storage-delete and row-delete).
//
// Layers deferred to E2d.2.b: keep-last-N per pipeline, per-project
// soft quota, global hard quota.
package retention

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Defaults picked conservatively: a 10-minute tick puts the p95
// retention drift at under 10 minutes, which is fine for byte-budget
// purposes; 500 per batch keeps the backend delete bounded on S3's
// 1000/request cap; 5-minute grace lets a normal tick finish before a
// retry starts stealing work.
const (
	DefaultTick         = 10 * time.Minute
	DefaultBatchSize    = 500
	DefaultGraceMinutes = 5
)

// Sweeper is the long-running task. Call Run inside a goroutine; it
// blocks until ctx is cancelled.
type Sweeper struct {
	store   *store.Store
	storage artifacts.Store
	log     *slog.Logger

	tick         time.Duration
	batchSize    int
	graceMinutes int
}

// New wires a Sweeper. nil Store is a programming error (panic via
// use). nil storage is a soft disable — Run() logs + exits early so
// deployments without artefact backend don't spam errors.
func New(s *store.Store, storage artifacts.Store, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{
		store:        s,
		storage:      storage,
		log:          log,
		tick:         DefaultTick,
		batchSize:    DefaultBatchSize,
		graceMinutes: DefaultGraceMinutes,
	}
}

// WithTick overrides the tick interval. Mainly for tests.
func (s *Sweeper) WithTick(d time.Duration) *Sweeper {
	if d > 0 {
		s.tick = d
	}
	return s
}

// WithBatchSize overrides the per-tick cap.
func (s *Sweeper) WithBatchSize(n int) *Sweeper {
	if n > 0 {
		s.batchSize = n
	}
	return s
}

// WithGraceMinutes overrides the retry grace for rows stuck in
// 'deleting' (how long we wait before assuming a previous sweeper
// attempt crashed and re-claiming the row).
func (s *Sweeper) WithGraceMinutes(n int) *Sweeper {
	if n > 0 {
		s.graceMinutes = n
	}
	return s
}

// Run blocks until ctx is cancelled. Runs one sweep immediately, then
// on each tick.
func (s *Sweeper) Run(ctx context.Context) error {
	if s.storage == nil {
		s.log.Info("retention: no artifact backend configured; sweeper disabled")
		<-ctx.Done()
		return nil
	}
	s.log.Info("retention: sweeper started",
		"tick", s.tick, "batch", s.batchSize, "grace_minutes", s.graceMinutes)

	// First sweep on start so ops can boot with a pending backlog and
	// see progress immediately.
	s.SweepOnce(ctx)

	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.SweepOnce(ctx)
		}
	}
}

// SweepStats is what one tick produced. Exposed so ops/metrics can
// log or export it; the sweeper itself only logs aggregate numbers.
type SweepStats struct {
	Claimed         int
	Deleted         int
	StorageFailures int
	DBFailures      int
	BytesFreed      int64
}

// SweepOnce runs a single batch and returns what happened. Exported so
// tests can drive the sweep without a ticker.
func (s *Sweeper) SweepOnce(ctx context.Context) SweepStats {
	var stats SweepStats
	claimed, err := s.store.ClaimArtifactsForSweep(ctx, s.batchSize, s.graceMinutes)
	if err != nil {
		s.log.Warn("retention: claim failed", "err", err)
		return stats
	}
	stats.Claimed = len(claimed)
	if stats.Claimed == 0 {
		return stats
	}

	for _, row := range claimed {
		if err := s.storage.Delete(ctx, row.StorageKey); err != nil {
			if !errors.Is(err, artifacts.ErrNotFound) {
				s.log.Warn("retention: storage delete failed",
					"storage_key", row.StorageKey, "err", err)
				stats.StorageFailures++
				// Leave the row in 'deleting' — next tick after the
				// grace window retries it. Don't delete the DB row
				// because the object is still out there.
				continue
			}
			// ErrNotFound is fine: idempotent delete. Row can be
			// reaped.
		}
		if err := s.store.RemoveArtifactRow(ctx, row.ID); err != nil {
			s.log.Warn("retention: remove row failed",
				"artifact_id", row.ID, "err", err)
			stats.DBFailures++
			continue
		}
		stats.Deleted++
		stats.BytesFreed += row.SizeBytes
	}

	s.log.Info("retention: sweep done",
		"claimed", stats.Claimed,
		"deleted", stats.Deleted,
		"bytes_freed", stats.BytesFreed,
		"storage_failures", stats.StorageFailures,
		"db_failures", stats.DBFailures)
	return stats
}
