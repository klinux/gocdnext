package store

import (
	"context"
	"fmt"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// BuildTimeStats is one set of build-duration percentiles (seconds) over a
// window, plus the sample size they were computed from.
type BuildTimeStats struct {
	Builds int64   `json:"builds"`
	P50Sec float64 `json:"p50_seconds"`
	P85Sec float64 `json:"p85_seconds"`
	P95Sec float64 `json:"p95_seconds"`
}

// BuildTimeGroup is the build-duration percentiles for one label-value group.
type BuildTimeGroup struct {
	Group string `json:"group"`
	BuildTimeStats
}

// BuildTimesReport backs the analytics page's "build time" panel: org-wide
// percentiles plus a per-group breakdown, over the window. A "build" is a
// successful, non-PR, non-deploy run that produced a ready artifact — the
// pipelines that build the artifact/image, isolated from PR-validation noise and
// from deploy runs (whose duration isn't build time). See analytics_build.sql.
type BuildTimesReport struct {
	Key        string           `json:"key"`
	WindowDays int              `json:"window_days"`
	Overall    BuildTimeStats   `json:"overall"`
	Groups     []BuildTimeGroup `json:"groups"`
}

// BuildTimesReport computes build-duration percentiles for the label key over
// the trailing windowDays. Percentiles are live (not summable from the rollup).
func (s *Store) BuildTimesReport(ctx context.Context, labelKey string, windowDays int) (BuildTimesReport, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	wd := int32(windowDays)

	overall, err := s.q.BuildTimesOverall(ctx, db.BuildTimesOverallParams{LabelKey: labelKey, WindowDays: wd})
	if err != nil {
		return BuildTimesReport{}, fmt.Errorf("store: build times overall: %w", err)
	}
	groupRows, err := s.q.BuildTimesByGroup(ctx, db.BuildTimesByGroupParams{LabelKey: labelKey, WindowDays: wd})
	if err != nil {
		return BuildTimesReport{}, fmt.Errorf("store: build times by group: %w", err)
	}

	groups := make([]BuildTimeGroup, 0, len(groupRows))
	for _, g := range groupRows {
		groups = append(groups, BuildTimeGroup{
			Group: g.Grp,
			BuildTimeStats: BuildTimeStats{
				Builds: g.Builds,
				P50Sec: g.P50S,
				P85Sec: g.P85S,
				P95Sec: g.P95S,
			},
		})
	}

	return BuildTimesReport{
		Key:        labelKey,
		WindowDays: windowDays,
		Overall: BuildTimeStats{
			Builds: overall.Builds,
			P50Sec: overall.P50S,
			P85Sec: overall.P85S,
			P95Sec: overall.P95S,
		},
		Groups: groups,
	}, nil
}

// LabelKeysWithData returns the label keys worth offering as a group-by: only
// those on a project with a terminal run inside the window. Falls back to
// signalling "none" (empty slice) when no labelled project has activity — the
// caller decides whether to show all keys instead.
func (s *Store) LabelKeysWithData(ctx context.Context, windowDays int) ([]string, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	keys, err := s.q.ListLabelKeysWithData(ctx, int32(windowDays))
	if err != nil {
		return nil, fmt.Errorf("store: label keys with data: %w", err)
	}
	return keys, nil
}
