package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

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

	findings := []store.FindingIn{
		{Tool: "Trivy", RuleID: "CVE-1", Severity: "critical", Level: "error", Message: "m1", LocationPath: "go.sum", Fingerprint: "fp1", ArtifactPath: "trivy.sarif"},
		{Tool: "Semgrep", RuleID: "r-xss", Severity: "medium", Level: "warning", Message: "m2", LocationPath: "web/h.go", LocationLine: 42, Fingerprint: "fp2", ArtifactPath: "semgrep.sarif"},
		{Tool: "Semgrep", RuleID: "r-low", Severity: "low", Level: "note", Message: "m3", Fingerprint: "fp3", ArtifactPath: "semgrep.sarif"},
	}
	if err := s.ReplaceSecurityFindings(ctx, jobID, findings); err != nil {
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
	if err := s.ReplaceSecurityFindings(ctx, jobID, nil); err != nil {
		t.Fatalf("replace empty: %v", err)
	}
	cleared, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil || cleared.Total != 0 || len(cleared.Findings) != 0 {
		t.Fatalf("empty replace must clear, got total=%d (err %v)", cleared.Total, err)
	}
}
