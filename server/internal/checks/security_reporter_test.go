package checks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func TestRefreshSecuritySummary_ConvergesAndMarksCompleted(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()
	s := store.New(pool)

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	var jobRunID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM job_runs WHERE run_id=$1`, runID).Scan(&jobRunID); err != nil {
		t.Fatalf("job_run: %v", err)
	}
	if err := s.ReplaceSecurityFindings(ctx, jobRunID, 0, []store.FindingIn{
		{Tool: "T", RuleID: "r", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp1"},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runs SET status='success', finished_at=NOW() WHERE id=$1`, runID); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	stub.updatedBody.Store(nil)

	// Terminal + link.Completed=false → must complete + mark, not deadlock.
	if err := r.RefreshSecuritySummary(ctx, runID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	up := stub.updatedBody.Load()
	if up == nil {
		t.Fatal("no PATCH captured")
	}
	body := *up
	if body["status"] != "completed" || body["conclusion"] != "success" {
		t.Fatalf("refresh must reassert completed+conclusion, got status=%v conclusion=%v", body["status"], body["conclusion"])
	}
	out, _ := body["output"].(map[string]any)
	summary, _ := out["summary"].(string)
	if !strings.Contains(summary, "**Security**") || !strings.Contains(summary, "1 high open") {
		t.Fatalf("summary missing security line: %q", summary)
	}
	link, err := s.GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if !link.Completed {
		t.Fatalf("refresh that completed the check must mark github_check_runs.completed")
	}
}

func TestRefreshSecuritySummary_InProgressKeepsOpen(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("create: %v", err)
	}
	stub.updatedBody.Store(nil)

	// Run still running → refresh PATCHes output as in_progress, no conclusion,
	// and must NOT mark the link completed.
	if err := r.RefreshSecuritySummary(ctx, runID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	up := stub.updatedBody.Load()
	if up == nil {
		t.Fatal("no PATCH captured")
	}
	body := *up
	if body["status"] != "in_progress" {
		t.Fatalf("running refresh status = %v, want in_progress", body["status"])
	}
	if _, ok := body["conclusion"]; ok {
		t.Fatalf("running refresh must not send a conclusion, got %v", body["conclusion"])
	}
	link, err := store.New(pool).GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if link.Completed {
		t.Fatalf("a running refresh must not mark the check completed")
	}
}

func TestRefreshSecuritySummary_NoCheckNoOp(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	// No CreateCheck → no link → refresh is a silent no-op.
	if err := r.RefreshSecuritySummary(ctx, runID); err != nil {
		t.Fatalf("no-op should return nil: %v", err)
	}
	if stub.updatedBody.Load() != nil {
		t.Fatal("no PATCH should happen without a check link")
	}
}
