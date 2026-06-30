package store

import (
	"context"
	"errors"
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
	// Status is cross-run dedup: "new" (first seen in this run) or "existing".
	Status string `json:"status"`
}

// FixedFinding is an identity that was present in a prior scan but is gone from
// the scanner's latest reconciled run — rendered from the identity snapshot
// (the security_findings occurrence row no longer exists). ID is the
// security_finding_states identity id.
type FixedFinding struct {
	ID           int64     `json:"id"`
	PipelineID   uuid.UUID `json:"pipeline_id"`
	ScannerJob   string    `json:"scanner_job"`
	MatrixKey    string    `json:"matrix_key"`
	Tool         string    `json:"tool"`
	RuleID       string    `json:"rule_id"`
	Severity     string    `json:"severity"`
	Level        string    `json:"level"`
	Message      string    `json:"message"`
	LocationPath string    `json:"location_path"`
	LocationLine int       `json:"location_line"`
	LastSeenAt   time.Time `json:"last_seen_at"`
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
// counts for the header chips + the cross-run "fixed" set.
type FindingsPage struct {
	Findings       []Finding        `json:"findings"`
	Total          int64            `json:"total"`
	SeverityCounts map[string]int64 `json:"severity_counts"`
	Fixed          []FixedFinding   `json:"fixed"`
	FixedTotal     int64            `json:"fixed_total"`
	Limit          int32            `json:"limit"`
	Offset         int32            `json:"offset"`
}

// maxFixedFindings caps the "fixed since last scan" list. Fixed sets are small
// in practice (identities absent from the latest scan); the cap keeps the
// Security tab payload bounded if a scanner is removed and a huge prior set
// retires at once.
const maxFixedFindings = 200

// upsertFindingIdentitiesSQL batch-upserts one identity per current finding in a
// single statement. Kept as raw SQL (not sqlc) because sqlc's static analyzer
// can't model the multi-array unnest(a,b,...) FROM-form, which is valid at
// runtime. ON CONFLICT touches ONLY last_seen_* + the snapshot — first_seen_*
// and every state_* column are left intact, so a re-ingest can neither move
// first-seen nor clobber a user's dismiss/accept.
const upsertFindingIdentitiesSQL = `
INSERT INTO security_finding_states (
    project_id, pipeline_id, scanner_job, matrix_key, tool, fingerprint,
    first_seen_run_id, first_seen_at, last_seen_run_id, last_seen_at,
    last_rule_id, last_severity, last_level, last_message,
    last_location_path, last_location_line
)
SELECT
    $1, $2, $3, $4, t.tool, t.fingerprint,
    $5, NOW(), $5, NOW(),
    t.rule_id, t.severity, t.level, t.message, t.location_path, t.location_line
FROM unnest($6::text[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[], $12::text[], $13::int[])
    AS t(tool, fingerprint, rule_id, severity, level, message, location_path, location_line)
ON CONFLICT (pipeline_id, scanner_job, matrix_key, tool, fingerprint) DO UPDATE
    SET last_seen_run_id   = EXCLUDED.last_seen_run_id,
        last_seen_at       = NOW(),
        last_rule_id       = EXCLUDED.last_rule_id,
        last_severity      = EXCLUDED.last_severity,
        last_level         = EXCLUDED.last_level,
        last_message       = EXCLUDED.last_message,
        last_location_path = EXCLUDED.last_location_path,
        last_location_line = EXCLUDED.last_location_line`

// ReplaceSecurityFindings replaces all findings for a job_run with `findings`
// in one tx (DELETE + batch insert). Always run on a successful reconcile —
// including with an EMPTY slice, so a rerun that no longer emits SARIF clears
// the prior attempt's findings. Never call this on a read/parse error (that
// would silently hide vulnerabilities).
//
// Snapshot-CAS on `attempt`: ingestion is async, and RerunJob reuses the same
// job_run_id with a bumped attempt — so a slow goroutine from the PREVIOUS
// attempt must not clobber the new attempt's findings. Returns ErrSnapshotStale
// when the row's current attempt no longer matches expectedAttempt (or the row
// is gone); the caller drops the stale write.
func (s *Store) ReplaceSecurityFindings(ctx context.Context, jobRunID uuid.UUID, expectedAttempt int32, findings []FindingIn) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: findings begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	var rowAttempt int32
	if err := tx.QueryRow(ctx,
		`SELECT attempt FROM job_runs WHERE id = $1 FOR UPDATE`, jobRunID,
	).Scan(&rowAttempt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSnapshotStale
		}
		return fmt.Errorf("store: findings lock row: %w", err)
	}
	if rowAttempt != expectedAttempt {
		return ErrSnapshotStale
	}

	// Context (run/pipeline/project/job) is resolved unconditionally — needed
	// both to stamp findings and to write the reconciliation marker even for a
	// clean (zero-finding) scan.
	cx, err := q.SecurityFindingContext(ctx, pgUUID(jobRunID))
	if err != nil {
		return fmt.Errorf("store: findings context: %w", err)
	}

	if err := q.DeleteSecurityFindingsByJobRun(ctx, pgUUID(jobRunID)); err != nil {
		return fmt.Errorf("store: findings delete: %w", err)
	}

	// Guard: only insert occurrences + upsert identities when there's something
	// to write. A clean (zero-finding) scan still writes the marker below, which
	// is what advances "fixed" detection for the scanner.
	if len(findings) > 0 {
		n := len(findings)
		params := make([]db.InsertSecurityFindingsParams, 0, n)
		// Parallel arrays for the batch identity upsert — built in lockstep so
		// they're guaranteed the same length.
		tools := make([]string, n)
		fps := make([]string, n)
		rules := make([]string, n)
		sevs := make([]string, n)
		levels := make([]string, n)
		msgs := make([]string, n)
		paths := make([]string, n)
		lines := make([]int32, n)
		for i, f := range findings {
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
				MatrixKey:    cx.MatrixKey,
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
			tools[i] = f.Tool
			fps[i] = f.Fingerprint
			rules[i] = f.RuleID
			sevs[i] = f.Severity
			levels[i] = f.Level
			msgs[i] = f.Message
			paths[i] = f.LocationPath
			lines[i] = int32(f.LocationLine)
		}
		if _, err := q.InsertSecurityFindings(ctx, params); err != nil {
			return fmt.Errorf("store: findings insert: %w", err)
		}
		if _, err := tx.Exec(ctx, upsertFindingIdentitiesSQL,
			cx.ProjectID, cx.PipelineID, cx.JobName, cx.MatrixKey, cx.RunID,
			tools, fps, rules, sevs, levels, msgs, paths, lines,
		); err != nil {
			return fmt.Errorf("store: findings upsert identities: %w", err)
		}
	}

	// Reconciliation marker — the list only advances to this run once it's
	// recorded here, so a failed/in-flight scan never hides a prior run's
	// findings, and a clean scan (0 findings) is distinct from "not scanned".
	// scanner_job/matrix_key denormalize the scanner grain for the latest CTE.
	if err := q.UpsertSecurityScan(ctx, db.UpsertSecurityScanParams{
		JobRunID:     pgUUID(jobRunID),
		RunID:        cx.RunID,
		PipelineID:   cx.PipelineID,
		ScannerJob:   cx.JobName,
		MatrixKey:    cx.MatrixKey,
		FindingCount: int32(len(findings)),
	}); err != nil {
		return fmt.Errorf("store: findings mark scan: %w", err)
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
	fixedRows, err := s.q.FixedFindingsForProject(ctx, db.FixedFindingsForProjectParams{
		ProjectID: pid, Lim: maxFixedFindings,
	})
	if err != nil {
		return FindingsPage{}, fmt.Errorf("store: findings fixed: %w", err)
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
			Status:       r.Status,
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

	fixed := make([]FixedFinding, 0, len(fixedRows))
	for _, fr := range fixedRows {
		ff := FixedFinding{
			ID:           fr.ID,
			PipelineID:   fromPgUUID(fr.PipelineID),
			ScannerJob:   fr.ScannerJob,
			MatrixKey:    fr.MatrixKey,
			Tool:         fr.Tool,
			RuleID:       fr.LastRuleID,
			Severity:     fr.LastSeverity,
			Level:        fr.LastLevel,
			Message:      fr.LastMessage,
			LocationPath: fr.LastLocationPath,
			LocationLine: int(fr.LastLocationLine),
		}
		if fr.LastSeenAt.Valid {
			ff.LastSeenAt = fr.LastSeenAt.Time
		}
		fixed = append(fixed, ff)
	}

	return FindingsPage{
		Findings:       out,
		Total:          total,
		SeverityCounts: counts,
		Fixed:          fixed,
		FixedTotal:     int64(len(fixed)),
		Limit:          f.Limit,
		Offset:         f.Offset,
	}, nil
}
