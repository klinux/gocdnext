package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// MetricsWindowDays is the default rolling window for aggregate
// stats on the pipeline card. Seven days keeps the medians
// responsive to recent regressions without getting whipsawed by
// single bad runs (which one day of data would do).
const MetricsWindowDays = 7

// pipelineMetricsByID returns a pipeline_id → *PipelineMetrics map
// for every pipeline in the project with aggregate stats over the
// rolling window. Both the project-detail card footer and the VSM
// node overlay consume the same shape, so the lookup is hoisted
// here — two batched queries regardless of pipeline count.
func (s *Store) pipelineMetricsByID(
	ctx context.Context,
	slug string,
) (map[uuid.UUID]*PipelineMetrics, error) {
	window := intervalDays(MetricsWindowDays)
	out := map[uuid.UUID]*PipelineMetrics{}

	rows, err := s.q.PipelineMetricsByProjectSlug(ctx, db.PipelineMetricsByProjectSlugParams{
		Slug:        slug,
		SinceWindow: window,
	})
	if err != nil {
		return nil, fmt.Errorf("pipeline metrics: %w", err)
	}
	for _, r := range rows {
		if r.RunsConsidered == 0 {
			continue
		}
		pid := fromPgUUID(r.PipelineID)
		out[pid] = &PipelineMetrics{
			WindowDays:        MetricsWindowDays,
			RunsConsidered:    int(r.RunsConsidered),
			SuccessRate:       float64(r.Passed) / float64(r.RunsConsidered),
			LeadTimeP50Sec:    r.LeadTimeP50S,
			ProcessTimeP50Sec: r.ProcessTimeP50S,
		}
	}

	stageRows, err := s.q.PipelineStageMetricsByProjectSlug(ctx, db.PipelineStageMetricsByProjectSlugParams{
		Slug:        slug,
		SinceWindow: window,
	})
	if err != nil {
		return nil, fmt.Errorf("pipeline stage metrics: %w", err)
	}
	for _, r := range stageRows {
		pid := fromPgUUID(r.PipelineID)
		// A pipeline can have stage metrics without pipeline-level
		// metrics when only some runs reached terminal state at the
		// pipeline scope but stage_runs did — initialise on demand so
		// the per-stage call-outs still render.
		m, ok := out[pid]
		if !ok {
			m = &PipelineMetrics{WindowDays: MetricsWindowDays}
			out[pid] = m
		}
		stat := StageStat{
			Name:           r.StageName,
			RunsConsidered: int(r.RunsConsidered),
			DurationP50Sec: r.DurationP50S,
		}
		if r.RunsConsidered > 0 {
			stat.SuccessRate = float64(r.Passed) / float64(r.RunsConsidered)
		}
		m.StageStats = append(m.StageStats, stat)
	}

	return out, nil
}

// attachPipelineMetrics (legacy signature used by project-detail)
// delegates to pipelineMetricsByID so both paths share one source
// of truth.
func (s *Store) attachPipelineMetrics(
	ctx context.Context,
	slug string,
	pipelines []PipelineSummary,
	pipelineIdx map[uuid.UUID]int,
) error {
	byID, err := s.pipelineMetricsByID(ctx, slug)
	if err != nil {
		return err
	}
	for pid, m := range byID {
		if idx, ok := pipelineIdx[pid]; ok {
			pipelines[idx].Metrics = m
		}
	}
	return nil
}

// intervalDays packs an integer day count into pgtype.Interval.
// Using Days (not Microseconds) keeps the value exact across DST
// transitions — Postgres computes `now() - interval` in calendar
// days, not fixed 86400s chunks.
func intervalDays(n int) pgtype.Interval {
	return pgtype.Interval{Days: int32(n), Valid: true}
}
