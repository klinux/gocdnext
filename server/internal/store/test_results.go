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

// TestResultStatus is the closed set the agent/server both know.
// Anything else coming off an XML parse gets clamped to
// "errored" so a weird report never lands `NULL` in the column.
type TestResultStatus string

const (
	TestStatusPassed  TestResultStatus = "passed"
	TestStatusFailed  TestResultStatus = "failed"
	TestStatusSkipped TestResultStatus = "skipped"
	TestStatusErrored TestResultStatus = "errored"
)

// TestResultIn is the write-side shape. Kept flat on purpose —
// the agent ships one list of these per job, the server loops
// them into InsertTestResult. SystemOut / SystemErr are cut off
// at the protobuf layer so they never get big enough to blow
// postgres's row size on a pathologically noisy test.
type TestResultIn struct {
	Suite          string
	Classname      string
	Name           string
	Status         TestResultStatus
	DurationMillis int64
	FailureType    string
	FailureMessage string
	FailureDetail  string
	SystemOut      string
	SystemErr      string
}

// WriteTestResults replaces the prior result rows for a job run
// (if any) and inserts every entry in `results`. The two
// operations run in one transaction so a partial failure rolls
// the whole batch back; a rerun that fails mid-insert doesn't
// leave a half-populated state behind.
func (s *Store) WriteTestResults(ctx context.Context, jobRunID uuid.UUID, results []TestResultIn) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: write test results: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := q.DeleteTestResultsByJobRun(ctx, pgUUID(jobRunID)); err != nil {
		return fmt.Errorf("store: write test results: clear: %w", err)
	}
	for _, r := range results {
		status := string(r.Status)
		switch r.Status {
		case TestStatusPassed, TestStatusFailed, TestStatusSkipped, TestStatusErrored:
			// ok
		default:
			status = string(TestStatusErrored)
		}
		if err := q.InsertTestResult(ctx, db.InsertTestResultParams{
			JobRunID:       pgUUID(jobRunID),
			Suite:          r.Suite,
			Classname:      r.Classname,
			Name:           r.Name,
			Status:         status,
			DurationMs:     r.DurationMillis,
			FailureType:    nullableString(r.FailureType),
			FailureMessage: nullableString(r.FailureMessage),
			FailureDetail:  nullableString(r.FailureDetail),
			SystemOut:      nullableString(r.SystemOut),
			SystemErr:      nullableString(r.SystemErr),
		}); err != nil {
			return fmt.Errorf("store: insert test result: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: write test results: commit: %w", err)
	}
	return nil
}

// TestResultSummary is per-job-run aggregate shown on the
// Tests tab cards — counts + total duration without shipping
// the full case list.
type TestResultSummary struct {
	JobRunID   uuid.UUID
	Total      int64
	Passed     int64
	Failed     int64
	Skipped    int64
	Errored    int64
	DurationMs int64
}

// TestResultCase is the read-side shape for a single case. Fields
// map 1:1 to the JUnit concepts without the XML framing, so the
// UI can render pass/fail badges + expand a failing case to its
// detail without another round-trip.
type TestResultCase struct {
	ID             uuid.UUID
	JobRunID       uuid.UUID
	Suite          string
	Classname      string
	Name           string
	Status         string
	DurationMs     int64
	FailureType    string
	FailureMessage string
	FailureDetail  string
}

// TestCaseHistoryEntry is one row of the flake-chasing history
// strip: last N executions of a single case across every run.
// Only surfaces the context the drawer needs — no failure
// detail / system output here; those load on the per-run page
// if the user clicks through.
type TestCaseHistoryEntry struct {
	ID             uuid.UUID
	RunID          uuid.UUID
	RunCounter     int64
	PipelineName   string
	ProjectSlug    string
	Status         string
	DurationMs     int64
	FailureMessage string
	CreatedAt      time.Time
}

// TestCaseHistory returns up to `limit` most-recent executions
// of the (classname, name) pair, newest first. Backs the Tests-
// tab history popover: "has this flaked in the last N runs?".
// The (classname, name, created_at DESC) index covers the
// lookup directly so a clicked case is cheap even on a large
// test_results table.
func (s *Store) TestCaseHistory(ctx context.Context, classname, name string, limit int32) ([]TestCaseHistoryEntry, error) {
	if limit <= 0 {
		limit = 14
	}
	rows, err := s.q.ListTestCaseHistory(ctx, db.ListTestCaseHistoryParams{
		Classname: classname,
		Name:      name,
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("store: test case history: %w", err)
	}
	out := make([]TestCaseHistoryEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, TestCaseHistoryEntry{
			ID:             fromPgUUID(r.ID),
			RunID:          fromPgUUID(r.RunID),
			RunCounter:     r.RunCounter,
			PipelineName:   r.PipelineName,
			ProjectSlug:    r.ProjectSlug,
			Status:         r.Status,
			DurationMs:     r.DurationMs,
			FailureMessage: stringValue(r.FailureMessage),
			CreatedAt:      r.CreatedAt.Time,
		})
	}
	return out, nil
}

// TestResultsByRun returns both the full case list and the
// aggregate tally per job_run for a given set of job_run_ids —
// callers scope it by the jobs in a RunDetail. Two queries run
// sequentially in one call so the UI layer has a single fetch
// to wait on.
func (s *Store) TestResultsByRun(ctx context.Context, jobRunIDs []uuid.UUID) ([]TestResultCase, []TestResultSummary, error) {
	if len(jobRunIDs) == 0 {
		return nil, nil, nil
	}
	pgIDs := make([]pgtype.UUID, 0, len(jobRunIDs))
	for _, id := range jobRunIDs {
		pgIDs = append(pgIDs, pgUUID(id))
	}

	rows, err := s.q.ListTestResultsByRun(ctx, pgIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list test results: %w", err)
	}
	cases := make([]TestResultCase, 0, len(rows))
	for _, r := range rows {
		cases = append(cases, TestResultCase{
			ID:             fromPgUUID(r.ID),
			JobRunID:       fromPgUUID(r.JobRunID),
			Suite:          r.Suite,
			Classname:      r.Classname,
			Name:           r.Name,
			Status:         r.Status,
			DurationMs:     r.DurationMs,
			FailureType:    stringValue(r.FailureType),
			FailureMessage: stringValue(r.FailureMessage),
			FailureDetail:  stringValue(r.FailureDetail),
		})
	}

	counts, err := s.q.CountTestResultsByJobRun(ctx, pgIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("store: count test results: %w", err)
	}
	summaries := make([]TestResultSummary, 0, len(counts))
	for _, c := range counts {
		summaries = append(summaries, TestResultSummary{
			JobRunID:   fromPgUUID(c.JobRunID),
			Total:      c.Total,
			Passed:     c.Passed,
			Failed:     c.Failed,
			Skipped:    c.Skipped,
			Errored:    c.Errored,
			DurationMs: c.DurationMs,
		})
	}
	return cases, summaries, nil
}
