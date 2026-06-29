package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
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

// TestFindingsForProject_NewUnscannedRunDoesNotHidePrior guards the MED: a newer
// run that hasn't been (successfully) scanned must NOT hide the previous scanned
// run's findings. The list sources from the latest SCANNED run per pipeline.
func TestFindingsForProject_NewUnscannedRunDoesNotHidePrior(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	fp := store.FingerprintFor("https://github.com/org/sec2", "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "sec2", Name: "sec2",
		Pipelines: []*domain.Pipeline{{
			Name:   "p1",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/sec2", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "scan", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	projectID := applied.ProjectID
	pipelineID := applied.Pipelines[0].PipelineID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	mkRun := func(mod int64, rev string) (runID, jobID uuid.UUID) {
		res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
			PipelineID: pipelineID, MaterialID: materialID, ModificationID: mod,
			Revision: rev, Branch: "main", Provider: "github", Delivery: "t", TriggeredBy: "system:test",
		})
		if err != nil {
			t.Fatalf("create run %d: %v", mod, err)
		}
		return res.RunID, res.JobRuns[0].ID
	}

	// Run 1: scanned with a critical finding.
	_, job1 := mkRun(1, "aaa")
	if err := s.ReplaceSecurityFindings(ctx, job1, 0, []store.FindingIn{
		{Tool: "Trivy", RuleID: "CVE-1", Severity: "critical", Level: "error", Message: "m", Fingerprint: "fp1"},
	}); err != nil {
		t.Fatalf("reconcile run1: %v", err)
	}

	// Run 2: newer, but its scan never reconciled (failed / in-flight).
	mkRun(2, "bbb")

	// The tab must still show run 1's critical — run 2 isn't a scanned run yet.
	page, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 1 || page.Findings[0].RuleID != "CVE-1" {
		t.Fatalf("unscanned newer run must not hide prior findings, got %+v", page.Findings)
	}
}

// TestFindingsForProject_PerScannerLatest guards the per-scanner grain: when a
// new run reconciles only one scanner (clean), a different scanner's finding
// from the previous run must remain — each scanner advances independently.
func TestFindingsForProject_PerScannerLatest(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	fp := store.FingerprintFor("https://github.com/org/sec3", "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "sec3", Name: "sec3",
		Pipelines: []*domain.Pipeline{{
			Name:   "p1",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/sec3", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "trivy", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}},
				{Name: "semgrep", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	projectID := applied.ProjectID
	pipelineID := applied.Pipelines[0].PipelineID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	mkRun := func(mod int64) uuid.UUID {
		res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
			PipelineID: pipelineID, MaterialID: materialID, ModificationID: mod,
			Revision: "r", Branch: "main", Provider: "github", Delivery: "t", TriggeredBy: "system:test",
		})
		if err != nil {
			t.Fatalf("create run %d: %v", mod, err)
		}
		return res.RunID
	}
	jobByName := func(runID uuid.UUID, name string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT id FROM job_runs WHERE run_id=$1 AND name=$2`, runID, name).Scan(&id); err != nil {
			t.Fatalf("job %s: %v", name, err)
		}
		return id
	}

	// Run 1: both scanners reconcile — semgrep finds something, trivy is clean.
	run1 := mkRun(1)
	if err := s.ReplaceSecurityFindings(ctx, jobByName(run1, "semgrep"), 0, []store.FindingIn{
		{Tool: "Semgrep", RuleID: "r-xss", Severity: "high", Level: "warning", Message: "m", Fingerprint: "fp1"},
	}); err != nil {
		t.Fatalf("reconcile semgrep r1: %v", err)
	}
	if err := s.ReplaceSecurityFindings(ctx, jobByName(run1, "trivy"), 0, nil); err != nil {
		t.Fatalf("reconcile trivy r1: %v", err)
	}

	// Run 2: only trivy reconciles (still clean); semgrep's scan never lands.
	run2 := mkRun(2)
	if err := s.ReplaceSecurityFindings(ctx, jobByName(run2, "trivy"), 0, nil); err != nil {
		t.Fatalf("reconcile trivy r2: %v", err)
	}

	// The semgrep finding from run 1 must still show.
	page, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 1 || page.Findings[0].RuleID != "r-xss" {
		t.Fatalf("per-scanner: semgrep finding must survive a trivy-only newer scan, got %+v", page.Findings)
	}
}
