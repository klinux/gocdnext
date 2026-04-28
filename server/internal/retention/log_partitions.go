package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// LogPartitionStats is what one log-lifecycle pass produced.
type LogPartitionStats struct {
	Created int
	Dropped int
}

// SweepLogPartitions runs both halves of the log_lines lifecycle:
//
//  1. Make sure every month from the current one through `monthsAhead`
//     has a partition, so the next agent INSERT lands somewhere.
//  2. If `retention` is non-zero, drop monthly children whose upper
//     bound is older than now-retention. DROP PARTITION is constant
//     time and writes no WAL — the whole point of partitioning.
//
// Both halves swallow per-partition errors and just log them: a
// transient pgx blip on one drop shouldn't keep the next tick from
// trying again.
func SweepLogPartitions(
	ctx context.Context,
	s *store.Store,
	now time.Time,
	monthsAhead int,
	retention time.Duration,
	log *slog.Logger,
) LogPartitionStats {
	if log == nil {
		log = slog.Default()
	}
	var stats LogPartitionStats

	// Ensure pass: current month + the next monthsAhead so an agent
	// streaming logs at midnight on the last day of a month doesn't
	// have to wait for the next tick before its INSERT can route.
	for i := 0; i <= monthsAhead; i++ {
		target := now.AddDate(0, i, 0)
		if err := s.EnsureLogPartition(ctx, target); err != nil {
			log.Warn("retention: ensure log partition failed",
				"month", target.Format("2006-01"), "err", err)
			continue
		}
		// We can't tell from EnsureLogPartition's signature whether
		// the partition was created or already existed — but the
		// daily cadence makes "Created" cheap to over-report; ops
		// just sees a small steady number.
		stats.Created++
	}

	// Drop pass: only when an explicit retention is configured. A
	// partition's upper bound is exclusive, so we drop when
	// upper <= cutoff (entire month falls outside the retention
	// window).
	if retention > 0 {
		cutoff := now.Add(-retention)
		parts, err := s.ListLogPartitions(ctx)
		if err != nil {
			log.Warn("retention: list log partitions failed", "err", err)
			return stats
		}
		for _, p := range parts {
			if !p.End.After(cutoff) {
				if err := s.DropLogPartition(ctx, p.Name); err != nil {
					log.Warn("retention: drop log partition failed",
						"name", p.Name, "err", err)
					continue
				}
				stats.Dropped++
				metrics.RetentionDroppedLogPartitions.Inc()
				log.Info("retention: log partition dropped",
					"name", p.Name, "end", p.End)
			}
		}
	}
	return stats
}
