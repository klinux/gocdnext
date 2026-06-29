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

// ReliabilityReport rolls up run throughput + reliability for labelKey over the
// trailing windowDays. Run-based — no environment filter (runs aren't
// environment-scoped). Success rate + runs/day are derived in Go from counts.
func (s *Store) ReliabilityReport(ctx context.Context, labelKey string, windowDays int) (ReliabilityReport, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	since := dayInterval(windowDays)

	rows, err := s.q.ThroughputRollup(ctx, db.ThroughputRollupParams{LabelKey: labelKey, SinceWindow: since})
	if err != nil {
		return ReliabilityReport{}, fmt.Errorf("store: throughput rollup: %w", err)
	}
	groups := make([]ThroughputGroup, 0, len(rows))
	for _, r := range rows {
		g := ThroughputGroup{
			Group:           r.Grp,
			RunsSuccess:     r.RunsSuccess,
			RunsFailed:      r.RunsFailed,
			RunsTotal:       r.RunsTotal,
			QueueWaitP50Sec: r.QueueWaitP50S,
			DurationP50Sec:  r.DurationP50S,
			RunsPerDay:      float64(r.RunsTotal) / float64(windowDays),
		}
		if r.RunsTotal > 0 {
			g.SuccessRate = float64(r.RunsSuccess) / float64(r.RunsTotal)
		}
		groups = append(groups, g)
	}

	hotRows, err := s.q.ReliabilityHotspots(ctx, db.ReliabilityHotspotsParams{
		LabelKey:    labelKey,
		SinceWindow: since,
		MinRuns:     reliabilityHotspotMinRuns,
		MaxRows:     reliabilityHotspotMaxRows,
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
