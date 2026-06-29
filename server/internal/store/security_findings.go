package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// FindingIn is one parsed SARIF finding to persist (the run/pipeline/project/job
// context is resolved from the job_run by ReplaceSecurityFindings).
type FindingIn struct {
	ArtifactID   uuid.UUID
	ArtifactPath string
	Tool         string
	RuleID       string
	Severity     string
	Level        string
	Message      string
	LocationPath string
	LocationLine int
	LocationURL  string
	Fingerprint  string
}

// Finding is a stored security finding for the project Security tab.
type Finding struct {
	ID           int64      `json:"id"`
	PipelineID   uuid.UUID  `json:"pipeline_id"`
	RunID        uuid.UUID  `json:"run_id"`
	JobName      string     `json:"job_name"`
	Tool         string     `json:"tool"`
	RuleID       string     `json:"rule_id"`
	Severity     string     `json:"severity"`
	Level        string     `json:"level"`
	Message      string     `json:"message"`
	LocationPath string     `json:"location_path"`
	LocationLine int        `json:"location_line"`
	LocationURL  string     `json:"location_url"`
	ArtifactID   *uuid.UUID `json:"artifact_id,omitempty"`
	ArtifactPath string     `json:"artifact_path"`
	CreatedAt    time.Time  `json:"created_at"`
}

// FindingsFilter narrows the findings list; empty strings disable a filter.
type FindingsFilter struct {
	Severity string
	Tool     string
	Rule     string
	Limit    int32
	Offset   int32
}

// FindingsPage is the paginated findings response + the (unfiltered) severity
// counts for the header chips.
type FindingsPage struct {
	Findings       []Finding        `json:"findings"`
	Total          int64            `json:"total"`
	SeverityCounts map[string]int64 `json:"severity_counts"`
	Limit          int32            `json:"limit"`
	Offset         int32            `json:"offset"`
}

// ReplaceSecurityFindings replaces all findings for a job_run with `findings`
// in one tx (DELETE + batch insert). Always run on a successful reconcile —
// including with an EMPTY slice, so a rerun that no longer emits SARIF clears
// the prior attempt's findings. Never call this on a read/parse error (that
// would silently hide vulnerabilities).
func (s *Store) ReplaceSecurityFindings(ctx context.Context, jobRunID uuid.UUID, findings []FindingIn) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: findings begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := q.DeleteSecurityFindingsByJobRun(ctx, pgUUID(jobRunID)); err != nil {
		return fmt.Errorf("store: findings delete: %w", err)
	}

	if len(findings) > 0 {
		cx, err := q.SecurityFindingContext(ctx, pgUUID(jobRunID))
		if err != nil {
			return fmt.Errorf("store: findings context: %w", err)
		}
		params := make([]db.InsertSecurityFindingsParams, 0, len(findings))
		for _, f := range findings {
			// artifact_id is nullable (ON DELETE SET NULL); Nil → SQL NULL so
			// the FK isn't violated when a finding has no artifact pointer.
			artID := pgtype.UUID{}
			if f.ArtifactID != uuid.Nil {
				artID = pgUUID(f.ArtifactID)
			}
			params = append(params, db.InsertSecurityFindingsParams{
				JobRunID:     pgUUID(jobRunID),
				RunID:        cx.RunID,
				PipelineID:   cx.PipelineID,
				ProjectID:    cx.ProjectID,
				JobName:      cx.JobName,
				ArtifactID:   artID,
				ArtifactPath: f.ArtifactPath,
				Tool:         f.Tool,
				RuleID:       f.RuleID,
				Severity:     f.Severity,
				Level:        f.Level,
				Message:      f.Message,
				LocationPath: f.LocationPath,
				LocationLine: int32(f.LocationLine),
				LocationUrl:  f.LocationURL,
				Fingerprint:  f.Fingerprint,
			})
		}
		if _, err := q.InsertSecurityFindings(ctx, params); err != nil {
			return fmt.Errorf("store: findings insert: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: findings commit: %w", err)
	}
	return nil
}

// FindingsForProject returns the findings from the latest run per pipeline in
// the project, filtered + paginated, plus the unfiltered severity counts.
func (s *Store) FindingsForProject(ctx context.Context, projectID uuid.UUID, f FindingsFilter) (FindingsPage, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	pid := pgUUID(projectID)

	rows, err := s.q.FindingsForProject(ctx, db.FindingsForProjectParams{
		ProjectID: pid, Severity: f.Severity, Tool: f.Tool, Rule: f.Rule,
		Lim: f.Limit, Off: f.Offset,
	})
	if err != nil {
		return FindingsPage{}, fmt.Errorf("store: findings list: %w", err)
	}
	total, err := s.q.CountFindingsForProject(ctx, db.CountFindingsForProjectParams{
		ProjectID: pid, Severity: f.Severity, Tool: f.Tool, Rule: f.Rule,
	})
	if err != nil {
		return FindingsPage{}, fmt.Errorf("store: findings count: %w", err)
	}
	sevRows, err := s.q.SeverityCountsForProject(ctx, pid)
	if err != nil {
		return FindingsPage{}, fmt.Errorf("store: findings severity counts: %w", err)
	}

	out := make([]Finding, 0, len(rows))
	for _, r := range rows {
		fnd := Finding{
			ID:           r.ID,
			PipelineID:   fromPgUUID(r.PipelineID),
			RunID:        fromPgUUID(r.RunID),
			JobName:      r.JobName,
			Tool:         r.Tool,
			RuleID:       r.RuleID,
			Severity:     r.Severity,
			Level:        r.Level,
			Message:      r.Message,
			LocationPath: r.LocationPath,
			LocationLine: int(r.LocationLine),
			LocationURL:  r.LocationUrl,
			ArtifactPath: r.ArtifactPath,
		}
		if r.ArtifactID.Valid {
			id := fromPgUUID(r.ArtifactID)
			fnd.ArtifactID = &id
		}
		if r.CreatedAt.Valid {
			fnd.CreatedAt = r.CreatedAt.Time
		}
		out = append(out, fnd)
	}

	counts := make(map[string]int64, len(sevRows))
	for _, c := range sevRows {
		counts[c.Severity] = c.N
	}

	return FindingsPage{
		Findings:       out,
		Total:          total,
		SeverityCounts: counts,
		Limit:          f.Limit,
		Offset:         f.Offset,
	}, nil
}
