// Package analytics maintains materialized rollups that keep the cross-project
// analytics dashboard cheap to read as run history grows (#128 phase 1).
package analytics

import (
	"context"
	"log/slog"
	"time"
)

// Refresher keeps the analytics_run_daily rollup current: a full backfill on
// boot, then an incremental refresh of the trailing few days each tick (so
// late-finishing runs and same-day runs land within one interval). The dashboard
// reads the rollup, never the loop — a missed tick just means slightly stale
// counts, never wrong ones (the next refresh recomputes whole days).
type Refresher struct {
	store        RollupStore
	log          *slog.Logger
	tick         time.Duration
	trailingDays int
}

// RollupStore is the slice of the store the refresher needs.
type RollupStore interface {
	RefreshRunDaily(ctx context.Context, sinceDays int) error
}

// NewRefresher builds a refresher with sensible defaults: refresh every 5
// minutes, recomputing the trailing 2 calendar days.
func NewRefresher(store RollupStore, log *slog.Logger) *Refresher {
	return &Refresher{store: store, log: log, tick: 5 * time.Minute, trailingDays: 2}
}

// Run backfills all history once, then refreshes the trailing window on each
// tick until ctx is cancelled. Errors are logged, never fatal — a stale rollup
// is recoverable on the next tick.
func (r *Refresher) Run(ctx context.Context) error {
	r.log.Info("analytics rollup refresher started", "tick", r.tick, "trailing_days", r.trailingDays)

	// Boot backfill (sinceDays <= 0 → all history). At large history this is a
	// single grouped upsert per boot; an incremental watermark is the lever if
	// boots get expensive (#128).
	if err := r.store.RefreshRunDaily(ctx, 0); err != nil {
		r.log.Error("analytics rollup backfill", "err", err)
	}

	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := r.store.RefreshRunDaily(ctx, r.trailingDays); err != nil {
				r.log.Error("analytics rollup refresh", "err", err)
			}
		}
	}
}
