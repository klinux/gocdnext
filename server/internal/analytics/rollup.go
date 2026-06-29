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
	fullEvery    time.Duration
	trailingDays int
}

// RollupStore is the slice of the store the refresher needs. RefreshRunDaily is
// leader-gated internally (advisory lock), so running it on every replica is
// safe — only one does the work per cycle.
type RollupStore interface {
	RefreshRunDaily(ctx context.Context, sinceDays int) error
}

// NewRefresher builds a refresher with sensible defaults: refresh the trailing 2
// days every 5 minutes (freshness), and a full rebuild every hour (heals reruns
// of runs that finished outside the trailing window).
func NewRefresher(store RollupStore, log *slog.Logger) *Refresher {
	return &Refresher{
		store:        store,
		log:          log,
		tick:         5 * time.Minute,
		fullEvery:    time.Hour,
		trailingDays: 2,
	}
}

// Run rebuilds all history once on boot, then on each tick refreshes the
// trailing window (freshness) and, less often, rebuilds in full (self-healing).
// Errors are logged, never fatal — a stale rollup recovers on the next cycle.
func (r *Refresher) Run(ctx context.Context) error {
	r.log.Info("analytics rollup refresher started",
		"tick", r.tick, "full_every", r.fullEvery, "trailing_days", r.trailingDays)

	// Boot full rebuild (sinceDays <= 0 → all history). At large history this is
	// a single grouped scan per boot/hour; an incremental watermark is the lever
	// if it gets expensive (#128).
	if err := r.store.RefreshRunDaily(ctx, 0); err != nil {
		r.log.Error("analytics rollup backfill", "err", err)
	}

	t := time.NewTicker(r.tick)
	defer t.Stop()
	full := time.NewTicker(r.fullEvery)
	defer full.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := r.store.RefreshRunDaily(ctx, r.trailingDays); err != nil {
				r.log.Error("analytics rollup refresh", "err", err)
			}
		case <-full.C:
			if err := r.store.RefreshRunDaily(ctx, 0); err != nil {
				r.log.Error("analytics rollup full rebuild", "err", err)
			}
		}
	}
}
