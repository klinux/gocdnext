package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestSecurityFindings_ReplaceListFilterClear(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, _ := seedRunningAgentJob(t, pool)
	var projectID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT p.project_id FROM job_runs j
		JOIN runs r ON r.id = j.run_id
		JOIN pipelines p ON p.id = r.pipeline_id
		WHERE j.id = $1`, jobID).Scan(&projectID); err != nil {
		t.Fatalf("lookup project: %v", err)
	}

	markRunTerminal(t, pool, ctx, jobID)

	findings := []store.FindingIn{
		{Tool: "Trivy", RuleID: "CVE-1", Severity: "critical", Level: "error", Message: "m1", LocationPath: "go.sum", Fingerprint: "fp1", ArtifactPath: "trivy.sarif"},
		{Tool: "Semgrep", RuleID: "r-xss", Severity: "medium", Level: "warning", Message: "m2", LocationPath: "web/h.go", LocationLine: 42, Fingerprint: "fp2", ArtifactPath: "semgrep.sarif"},
		{Tool: "Semgrep", RuleID: "r-low", Severity: "low", Level: "note", Message: "m3", Fingerprint: "fp3", ArtifactPath: "semgrep.sarif"},
	}
	if err := s.ReplaceSecurityFindings(ctx, jobID, 0, findings); err != nil {
		t.Fatalf("replace: %v", err)
	}

	page, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 3 || len(page.Findings) != 3 {
		t.Fatalf("total/len = %d/%d, want 3/3", page.Total, len(page.Findings))
	}
	// Worst-severity first.
	if page.Findings[0].Severity != "critical" || page.Findings[2].Severity != "low" {
		t.Fatalf("ordering = %v", []string{page.Findings[0].Severity, page.Findings[1].Severity, page.Findings[2].Severity})
	}
	if page.SeverityCounts["critical"] != 1 || page.SeverityCounts["medium"] != 1 || page.SeverityCounts["low"] != 1 {
		t.Fatalf("severity counts = %+v", page.SeverityCounts)
	}

	// Filter by severity.
	med, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{Severity: "medium"})
	if err != nil || med.Total != 1 || med.Findings[0].RuleID != "r-xss" {
		t.Fatalf("severity filter = %+v (err %v)", med, err)
	}
	// Filter by tool.
	semg, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{Tool: "Semgrep"})
	if err != nil || semg.Total != 2 {
		t.Fatalf("tool filter total = %d, want 2 (err %v)", semg.Total, err)
	}

	// Replace with EMPTY must clear (a rerun that no longer emits SARIF).
	if err := s.ReplaceSecurityFindings(ctx, jobID, 0, nil); err != nil {
		t.Fatalf("replace empty: %v", err)
	}
	cleared, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil || cleared.Total != 0 || len(cleared.Findings) != 0 {
		t.Fatalf("empty replace must clear, got total=%d (err %v)", cleared.Total, err)
	}
}

func TestReplaceSecurityFindings_StaleAttemptRejected(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID, _, _ := seedRunningAgentJob(t, pool)
	var projectID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT p.project_id FROM job_runs j
		JOIN runs r ON r.id = j.run_id
		JOIN pipelines p ON p.id = r.pipeline_id
		WHERE j.id = $1`, jobID).Scan(&projectID); err != nil {
		t.Fatalf("lookup project: %v", err)
	}

	markRunTerminal(t, pool, ctx, jobID)

	// Attempt 0 writes one finding.
	if err := s.ReplaceSecurityFindings(ctx, jobID, 0, []store.FindingIn{
		{Tool: "Trivy", RuleID: "CVE-1", Severity: "critical", Level: "error", Message: "m", Fingerprint: "fp1"},
	}); err != nil {
		t.Fatalf("replace attempt 0: %v", err)
	}

	// A rerun bumps the attempt.
	if _, err := pool.Exec(ctx, `UPDATE job_runs SET attempt = 1 WHERE id = $1`, jobID); err != nil {
		t.Fatalf("bump attempt: %v", err)
	}

	// A slow goroutine from the OLD attempt (0) must be rejected — not clear the
	// new attempt's findings.
	if err := s.ReplaceSecurityFindings(ctx, jobID, 0, nil); !errors.Is(err, store.ErrSnapshotStale) {
		t.Fatalf("stale write err = %v, want ErrSnapshotStale", err)
	}
	page, _ := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if page.Total != 1 {
		t.Fatalf("stale write must NOT clear findings, total=%d", page.Total)
	}

	// The current attempt (1) writes normally.
	if err := s.ReplaceSecurityFindings(ctx, jobID, 1, []store.FindingIn{
		{Tool: "Semgrep", RuleID: "r-1", Severity: "high", Level: "warning", Message: "m2", Fingerprint: "fp2"},
		{Tool: "Semgrep", RuleID: "r-2", Severity: "low", Level: "note", Message: "m3", Fingerprint: "fp3"},
	}); err != nil {
		t.Fatalf("replace attempt 1: %v", err)
	}
	page, _ = s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if page.Total != 2 {
		t.Fatalf("current attempt write total = %d, want 2", page.Total)
	}
}

// markRunTerminal flips the job's run to a terminal status so it qualifies as
// the "latest terminal run per pipeline" the findings query reads from.
func markRunTerminal(t *testing.T, pool *pgxpool.Pool, ctx context.Context, jobRunID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='success' WHERE id = (SELECT run_id FROM job_runs WHERE id=$1)`,
		jobRunID); err != nil {
		t.Fatalf("mark run terminal: %v", err)
	}
}
