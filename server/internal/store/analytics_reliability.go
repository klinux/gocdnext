package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// Reliability hotspot bounds: a pipeline needs at least this many terminal runs
// to qualify (so a 1-of-1 failure doesn't top the list), and we surface at most
// this many worst offenders.
const (
	reliabilityHotspotMinRuns = 5
	reliabilityHotspotMaxRows = 8
	// rollupAdvisoryLockKey namespaces the analytics_run_daily refresh advisory
	// lock so only one replica rebuilds at a time. Arbitrary but fixed; distinct
	// from goose's migration lock.
	rollupAdvisoryLockKey = 4128937
)

// ThroughputGroup is the run-based throughput + reliability rollup for one
// label-value group over the window. Run-based, so no environment dimension.
// Counts come from the analytics_run_daily rollup; the p50s are computed live.
type ThroughputGroup struct {
	Group           string  `json:"group"`
	RunsSuccess     int64   `json:"runs_success"`
	RunsFailed      int64   `json:"runs_failed"`
	RunsTotal       int64   `json:"runs_total"`
	RunsPerDay      float64 `json:"runs_per_day"`
	SuccessRate     float64 `json:"success_rate"`
	QueueWaitP50Sec float64 `json:"queue_wait_p50_seconds"`
	DurationP50Sec  float64 `json:"duration_p50_seconds"`
}

// ReliabilityHotspot is one pipeline that fails often, among labelled projects.
type ReliabilityHotspot struct {
	ProjectSlug string  `json:"project_slug"`
	Project     string  `json:"project"`
	Pipeline    string  `json:"pipeline"`
	RunsTotal   int64   `json:"runs_total"`
	RunsFailed  int64   `json:"runs_failed"`
	FailureRate float64 `json:"failure_rate"`
}

// ReliabilityReport is the throughput/reliability payload behind the analytics
// page's "throughput & reliability" section: per-group throughput plus the
// org-wide reliability hotspots (worst-failing pipelines among labelled projects).
type ReliabilityReport struct {
	Key        string               `json:"key"`
	WindowDays int                  `json:"window_days"`
	Groups     []ThroughputGroup    `json:"groups"`
	Hotspots   []ReliabilityHotspot `json:"hotspots"`
}

// RefreshRunDaily rebuilds the analytics_run_daily rollup for the trailing
// sinceDays whole calendar days (sinceDays <= 0 → all history, the boot/periodic
// full rebuild). DELETE-then-reinsert in one tx — so buckets that lost their
// last terminal run go to zero and reruns that moved a run's status/day within
// the window self-correct (an additive upsert would double-count). A
// transaction-scoped advisory lock makes it leader-only across replicas; a
// replica that loses the lock skips this cycle (no-op, not an error). Reruns of
// runs that finished OUTSIDE the trailing window are healed by the periodic full
// rebuild.
func (s *Store) RefreshRunDaily(ctx context.Context, sinceDays int) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: refresh run daily begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	got, err := q.TryRollupLock(ctx, rollupAdvisoryLockKey)
	if err != nil {
		return fmt.Errorf("store: rollup lock: %w", err)
	}
	if !got {
		return nil // another replica is refreshing
	}

	if err := q.DeleteRunDailyWindow(ctx, int32(sinceDays)); err != nil {
		return fmt.Errorf("store: refresh run daily delete: %w", err)
	}
	if err := q.InsertRunDailyWindow(ctx, int32(sinceDays)); err != nil {
		return fmt.Errorf("store: refresh run daily insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: refresh run daily commit: %w", err)
	}
	return nil
}

// ReliabilityReport rolls up run throughput + reliability for labelKey over the
// trailing windowDays. Counts + hotspots read the materialized daily rollup
// (additive, O(days)); the queue/duration p50s are computed live. Run-based —
// no environment filter (runs aren't environment-scoped).
func (s *Store) ReliabilityReport(ctx context.Context, labelKey string, windowDays int) (ReliabilityReport, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	wd := int32(windowDays)

	counts, err := s.q.ThroughputCounts(ctx, db.ThroughputCountsParams{LabelKey: labelKey, WindowDays: wd})
	if err != nil {
		return ReliabilityReport{}, fmt.Errorf("store: throughput counts: %w", err)
	}
	latRows, err := s.q.ThroughputLatency(ctx, db.ThroughputLatencyParams{LabelKey: labelKey, WindowDays: wd})
	if err != nil {
		return ReliabilityReport{}, fmt.Errorf("store: throughput latency: %w", err)
	}
	lat := make(map[string]db.ThroughputLatencyRow, len(latRows))
	for _, l := range latRows {
		lat[l.Grp] = l
	}

	groups := make([]ThroughputGroup, 0, len(counts))
	for _, c := range counts {
		total := c.RunsSuccess + c.RunsFailed
		g := ThroughputGroup{
			Group:           c.Grp,
			RunsSuccess:     c.RunsSuccess,
			RunsFailed:      c.RunsFailed,
			RunsTotal:       total,
			RunsPerDay:      float64(total) / float64(windowDays),
			QueueWaitP50Sec: lat[c.Grp].QueueWaitP50S,
			DurationP50Sec:  lat[c.Grp].DurationP50S,
		}
		if total > 0 {
			g.SuccessRate = float64(c.RunsSuccess) / float64(total)
		}
		groups = append(groups, g)
	}

	hotRows, err := s.q.ReliabilityHotspots(ctx, db.ReliabilityHotspotsParams{
		LabelKey:   labelKey,
		WindowDays: wd,
		MinRuns:    reliabilityHotspotMinRuns,
		MaxRows:    reliabilityHotspotMaxRows,
	})
	if err != nil {
		return ReliabilityReport{}, fmt.Errorf("store: reliability hotspots: %w", err)
	}
	hotspots := make([]ReliabilityHotspot, 0, len(hotRows))
	for _, h := range hotRows {
		hotspots = append(hotspots, ReliabilityHotspot{
			ProjectSlug: h.ProjectSlug,
			Project:     h.Project,
			Pipeline:    h.Pipeline,
			RunsTotal:   h.RunsTotal,
			RunsFailed:  h.RunsFailed,
			FailureRate: h.FailureRate,
		})
	}

	return ReliabilityReport{
		Key:        labelKey,
		WindowDays: windowDays,
		Groups:     groups,
		Hotspots:   hotspots,
	}, nil
}
