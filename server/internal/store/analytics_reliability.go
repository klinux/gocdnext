package store

import (
	"context"
	"fmt"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// Reliability hotspot bounds: a pipeline needs at least this many terminal runs
// to qualify (so a 1-of-1 failure doesn't top the list), and we surface at most
// this many worst offenders.
const (
	reliabilityHotspotMinRuns = 5
	reliabilityHotspotMaxRows = 8
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

// RefreshRunDaily recomputes the analytics_run_daily rollup for the trailing
// sinceDays whole calendar days (sinceDays <= 0 → all history, the boot
// backfill). Idempotent — safe to re-run / overlap; catches late-finishing runs.
func (s *Store) RefreshRunDaily(ctx context.Context, sinceDays int) error {
	if err := s.q.RefreshRunDaily(ctx, int32(sinceDays)); err != nil {
		return fmt.Errorf("store: refresh run daily: %w", err)
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
