package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// PackageCoverageIn mirrors the proto breakdown entry — persisted
// verbatim into the JSONB column.
type PackageCoverageIn struct {
	Name         string `json:"name"`
	LinesCovered int64  `json:"lines_covered"`
	LinesTotal   int64  `json:"lines_total"`
}

// CoverageIn is the agent-reported summary for one job run.
type CoverageIn struct {
	Format       string
	LinesCovered int64
	LinesTotal   int64
	Packages     []PackageCoverageIn
}

// WriteCoverage persists one job run's coverage summary with the
// same snapshot-CAS contract WriteTestResults uses: the (agent,
// attempt) pair the gRPC handler observed must still be on the row,
// otherwise the write is stale (a rerun raced it) and is dropped
// loudly by the caller.
func (s *Store) WriteCoverage(ctx context.Context, jobRunID, expectedAgentID uuid.UUID, expectedAttempt int32, in CoverageIn) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: write coverage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	var rowAgent pgtype.UUID
	var rowAttempt int32
	if err := tx.QueryRow(ctx,
		`SELECT agent_id, attempt FROM job_runs WHERE id = $1 FOR UPDATE`, jobRunID,
	).Scan(&rowAgent, &rowAttempt); err != nil {
		if err == pgx.ErrNoRows {
			return ErrSnapshotStale
		}
		return fmt.Errorf("store: write coverage: lock row: %w", err)
	}
	if fromPgUUID(rowAgent) != expectedAgentID || rowAttempt != expectedAttempt {
		return ErrSnapshotStale
	}

	pkgs, err := json.Marshal(in.Packages)
	if err != nil {
		return fmt.Errorf("store: write coverage: marshal packages: %w", err)
	}
	if err := q.UpsertCoverageReport(ctx, db.UpsertCoverageReportParams{
		ID:           pgUUID(jobRunID),
		Format:       in.Format,
		LinesCovered: in.LinesCovered,
		LinesTotal:   in.LinesTotal,
		Packages:     pkgs,
	}); err != nil {
		return fmt.Errorf("store: upsert coverage: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: write coverage: commit: %w", err)
	}
	return nil
}

// CoverageRow is one job's summary as served to the run page.
// Baseline carries the latest push-run (mainline) measurement of
// the SAME series, when one exists — the UI derives the delta from
// the two raw pairs so percentage rounding stays in one place.
type CoverageRow struct {
	JobRunID     uuid.UUID           `json:"job_run_id"`
	JobName      string              `json:"job_name"`
	MatrixKey    string              `json:"matrix_key,omitempty"`
	Format       string              `json:"format"`
	LinesCovered int64               `json:"lines_covered"`
	LinesTotal   int64               `json:"lines_total"`
	Packages     []PackageCoverageIn `json:"packages,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
	Baseline     *CoverageBaseline   `json:"baseline,omitempty"`
}

// CoverageBaseline is the mainline comparison point for one series.
type CoverageBaseline struct {
	RunID        uuid.UUID `json:"run_id"`
	LinesCovered int64     `json:"lines_covered"`
	LinesTotal   int64     `json:"lines_total"`
}

// CoverageByRun lists every coverage summary of a run, each row
// annotated with its series' mainline baseline (latest push-run
// coverage, excluding this run) when one exists.
func (s *Store) CoverageByRun(ctx context.Context, runID uuid.UUID) ([]CoverageRow, error) {
	rows, err := s.q.CoverageByRun(ctx, pgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("store: coverage by run: %w", err)
	}
	baselines := map[string]CoverageBaseline{}
	if len(rows) > 0 {
		var pipelineID uuid.UUID
		if err := s.pool.QueryRow(ctx,
			`SELECT pipeline_id FROM runs WHERE id = $1`, runID,
		).Scan(&pipelineID); err == nil {
			if base, err := s.q.CoverageBaselineByPipeline(ctx, db.CoverageBaselineByPipelineParams{
				PipelineID: pgUUID(pipelineID),
				RunID:      pgUUID(runID),
			}); err == nil {
				for _, b := range base {
					baselines[b.JobName+"\x00"+b.MatrixKey] = CoverageBaseline{
						RunID:        fromPgUUID(b.RunID),
						LinesCovered: b.LinesCovered,
						LinesTotal:   b.LinesTotal,
					}
				}
			} else {
				// Baseline is enrichment, not the payload — a failed
				// lookup degrades to "no delta", never to a 500.
				_ = err
			}
		}
	}
	out := make([]CoverageRow, 0, len(rows))
	for _, r := range rows {
		row := CoverageRow{
			JobRunID:     fromPgUUID(r.JobRunID),
			JobName:      r.JobName,
			MatrixKey:    r.MatrixKey,
			Format:       r.Format,
			LinesCovered: r.LinesCovered,
			LinesTotal:   r.LinesTotal,
			CreatedAt:    r.CreatedAt.Time,
		}
		if len(r.Packages) > 0 {
			// Defensive: a malformed JSONB row degrades to "no
			// breakdown", never to a 500 on the run page.
			_ = json.Unmarshal(r.Packages, &row.Packages)
		}
		if b, ok := baselines[r.JobName+"\x00"+r.MatrixKey]; ok {
			row.Baseline = &b
		}
		out = append(out, row)
	}
	return out, nil
}

// CoverageTrendPoint is one (run, job) coverage measurement for the
// per-pipeline sparkline.
type CoverageTrendPoint struct {
	RunID        uuid.UUID `json:"run_id"`
	JobName      string    `json:"job_name"`
	MatrixKey    string    `json:"matrix_key,omitempty"`
	LinesCovered int64     `json:"lines_covered"`
	LinesTotal   int64     `json:"lines_total"`
	CreatedAt    time.Time `json:"created_at"`
}

// CoverageTrend returns the newest `limit` points for a pipeline,
// newest first (the UI flips for charting).
func (s *Store) CoverageTrend(ctx context.Context, pipelineID uuid.UUID, limit int32) ([]CoverageTrendPoint, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.q.CoverageTrendByPipeline(ctx, db.CoverageTrendByPipelineParams{
		PipelineID: pgUUID(pipelineID),
		Limit:      limit,
	})
	if err != nil {
		return nil, fmt.Errorf("store: coverage trend: %w", err)
	}
	out := make([]CoverageTrendPoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, CoverageTrendPoint{
			RunID:        fromPgUUID(r.RunID),
			JobName:      r.JobName,
			MatrixKey:    r.MatrixKey,
			LinesCovered: r.LinesCovered,
			LinesTotal:   r.LinesTotal,
			CreatedAt:    r.CreatedAt.Time,
		})
	}
	return out, nil
}
