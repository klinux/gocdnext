package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// scanProject applies a one-pipeline project with the named scanner jobs and
// returns the ids the cross-run tests need.
func scanProject(t *testing.T, pool *pgxpool.Pool, s *store.Store, slug string, jobs ...string) (projectID, pipelineID, materialID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	url := "https://github.com/org/" + slug
	fp := store.FingerprintFor(url, "main")
	js := make([]domain.Job, 0, len(jobs))
	for _, j := range jobs {
		js = append(js, domain.Job{Name: j, Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}})
	}
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "p1", Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: js,
		}},
	})
	if err != nil {
		t.Fatalf("apply %s: %v", slug, err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	return applied.ProjectID, applied.Pipelines[0].PipelineID, materialID
}

func newScanRun(t *testing.T, s *store.Store, pipelineID, materialID uuid.UUID, mod int64) uuid.UUID {
	t.Helper()
	res, err := s.CreateRunFromModification(context.Background(), store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID, ModificationID: mod,
		Revision: "r", Branch: "main", Provider: "github", Delivery: "t", TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("create run %d: %v", mod, err)
	}
	return res.RunID
}

func jobRunByName(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM job_runs WHERE run_id=$1 AND name=$2`, runID, name).Scan(&id); err != nil {
		t.Fatalf("job_run %s: %v", name, err)
	}
	return id
}

// TestFindingsForProject_StatusNewThenExisting: a fingerprint first seen in a
// run reads as "new"; when it reappears in a later run it reads as "existing".
func TestFindingsForProject_StatusNewThenExisting(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID, pipelineID, materialID := scanProject(t, pool, s, "sec-status", "scan")

	finding := store.FindingIn{Tool: "Trivy", RuleID: "CVE-1", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp1"}

	run1 := newScanRun(t, s, pipelineID, materialID, 1)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run1, "scan"), 0, []store.FindingIn{finding}); err != nil {
		t.Fatalf("reconcile r1: %v", err)
	}
	p1, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil || len(p1.Findings) != 1 {
		t.Fatalf("r1 list: %+v (err %v)", p1, err)
	}
	if p1.Findings[0].Status != "new" {
		t.Fatalf("first appearance must be new, got %q", p1.Findings[0].Status)
	}

	run2 := newScanRun(t, s, pipelineID, materialID, 2)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run2, "scan"), 0, []store.FindingIn{finding}); err != nil {
		t.Fatalf("reconcile r2: %v", err)
	}
	p2, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil || len(p2.Findings) != 1 {
		t.Fatalf("r2 list: %+v (err %v)", p2, err)
	}
	if p2.Findings[0].Status != "existing" {
		t.Fatalf("re-seen fingerprint must be existing, got %q", p2.Findings[0].Status)
	}
}

// TestFindingsForProject_StateFiltering: the default list shows open + accepted;
// dismissed + false_positive are hidden unless include_resolved. Severity counts
// cover open only; accepted is counted separately.
func TestFindingsForProject_StateFiltering(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID, pipelineID, materialID := scanProject(t, pool, s, "sec-statefilter", "scan")

	findings := []store.FindingIn{
		{Tool: "T", RuleID: "open-r", Severity: "critical", Level: "error", Message: "m", Fingerprint: "fp-open"},
		{Tool: "T", RuleID: "accepted-r", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-accepted"},
		{Tool: "T", RuleID: "dismissed-r", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-dismissed"},
		{Tool: "T", RuleID: "fp-r", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-falsepos"},
	}
	run1 := newScanRun(t, s, pipelineID, materialID, 1)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run1, "scan"), 0, findings); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for fp, st := range map[string]string{"fp-accepted": "accepted", "fp-dismissed": "dismissed", "fp-falsepos": "false_positive"} {
		if _, err := pool.Exec(ctx, `UPDATE security_finding_states SET state=$1 WHERE fingerprint=$2`, st, fp); err != nil {
			t.Fatalf("set state %s: %v", st, err)
		}
	}

	// Default: open + accepted only.
	def, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil {
		t.Fatalf("default list: %v", err)
	}
	if def.Total != 2 {
		t.Fatalf("default total = %d, want 2 (open + accepted)", def.Total)
	}
	rules := map[string]bool{}
	for _, f := range def.Findings {
		rules[f.RuleID] = true
	}
	if !rules["open-r"] || !rules["accepted-r"] || rules["dismissed-r"] || rules["fp-r"] {
		t.Fatalf("default must show open+accepted only, got %+v", rules)
	}
	// Severity counts cover OPEN only (the one critical), not the accepted high.
	if def.SeverityCounts["critical"] != 1 || def.SeverityCounts["high"] != 0 {
		t.Fatalf("severity counts must be open-only, got %+v", def.SeverityCounts)
	}
	if def.AcceptedCount != 1 {
		t.Fatalf("accepted_count = %d, want 1", def.AcceptedCount)
	}

	// include_resolved: all four.
	all, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{IncludeResolved: true})
	if err != nil {
		t.Fatalf("include_resolved list: %v", err)
	}
	if all.Total != 4 {
		t.Fatalf("include_resolved total = %d, want 4", all.Total)
	}
}

// TestReplaceSecurityFindings_DuplicateIdentityInScan guards the MED: a SARIF
// emitting two results that collapse to the same (tool, fingerprint) must not
// error the identity upsert (ON CONFLICT can't affect a row twice, which would
// roll back the whole reconcile and leave the dashboard stale). The scan
// reconciles, both occurrences are stored, the identity collapses to one.
func TestReplaceSecurityFindings_DuplicateIdentityInScan(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID, pipelineID, materialID := scanProject(t, pool, s, "sec-dup", "scan")

	dup := store.FindingIn{Tool: "Trivy", RuleID: "CVE-1", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-dup"}
	run1 := newScanRun(t, s, pipelineID, materialID, 1)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run1, "scan"), 0, []store.FindingIn{dup, dup}); err != nil {
		t.Fatalf("duplicate identity must reconcile, got: %v", err)
	}
	page, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("both occurrences must be stored, total=%d", page.Total)
	}
	var idents int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM security_finding_states WHERE fingerprint='fp-dup'`).Scan(&idents); err != nil {
		t.Fatalf("count idents: %v", err)
	}
	if idents != 1 {
		t.Fatalf("identity must collapse to one, got %d", idents)
	}
}

// TestFindingsForProject_FixedPerScannerNotTool is the make-or-break case: ONE
// scanner job emits two tools (Trivy + Semgrep) in run 1, then only Trivy in
// run 2. The Semgrep identities must surface as "fixed" even though
// security_scans carries no tool — fixed is per (pipeline, scanner_job, matrix),
// dedup/state is per identity (which includes tool).
func TestFindingsForProject_FixedPerScannerNotTool(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID, pipelineID, materialID := scanProject(t, pool, s, "sec-fixed", "scan")

	trivy := store.FindingIn{Tool: "Trivy", RuleID: "CVE-1", Severity: "critical", Level: "error", Message: "m", Fingerprint: "fp-trivy"}
	semgrep := store.FindingIn{Tool: "Semgrep", RuleID: "r-xss", Severity: "high", Level: "warning", Message: "m", LocationPath: "h.go", LocationLine: 9, Fingerprint: "fp-semgrep"}

	// Run 1: the scan job emits both tools.
	run1 := newScanRun(t, s, pipelineID, materialID, 1)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run1, "scan"), 0, []store.FindingIn{trivy, semgrep}); err != nil {
		t.Fatalf("reconcile r1: %v", err)
	}

	// Run 2: the same job emits only Trivy — Semgrep stopped emitting.
	run2 := newScanRun(t, s, pipelineID, materialID, 2)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run2, "scan"), 0, []store.FindingIn{trivy}); err != nil {
		t.Fatalf("reconcile r2: %v", err)
	}

	page, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 1 || page.Findings[0].RuleID != "CVE-1" {
		t.Fatalf("open list must be just the surviving Trivy finding, got %+v", page.Findings)
	}
	if page.FixedTotal != 1 || len(page.Fixed) != 1 || page.Fixed[0].RuleID != "r-xss" {
		t.Fatalf("the dropped Semgrep finding must be fixed, got %+v", page.Fixed)
	}
	if page.Fixed[0].Tool != "Semgrep" || page.Fixed[0].LocationPath != "h.go" || page.Fixed[0].LocationLine != 9 {
		t.Fatalf("fixed must render from the identity snapshot, got %+v", page.Fixed[0])
	}
}

// TestFixedFindings_RespectsState covers the state semantics of the fixed query:
// open + accepted surface as fixed; dismissed + false_positive never resurface.
// (The state-mutation endpoint is PR2; here we set the column directly.)
func TestFixedFindings_RespectsState(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID, pipelineID, materialID := scanProject(t, pool, s, "sec-fixed-state", "scan")

	// Run 1: four findings; Run 2: all gone (clean) → all four become fixed.
	fps := []store.FindingIn{
		{Tool: "T", RuleID: "open-r", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-open"},
		{Tool: "T", RuleID: "accepted-r", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-accepted"},
		{Tool: "T", RuleID: "dismissed-r", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-dismissed"},
		{Tool: "T", RuleID: "fp-r", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-falsepos"},
	}
	run1 := newScanRun(t, s, pipelineID, materialID, 1)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run1, "scan"), 0, fps); err != nil {
		t.Fatalf("reconcile r1: %v", err)
	}
	// Set states directly (PR1 has no mutation endpoint yet).
	for fp, st := range map[string]string{"fp-accepted": "accepted", "fp-dismissed": "dismissed", "fp-falsepos": "false_positive"} {
		if _, err := pool.Exec(ctx, `UPDATE security_finding_states SET state=$1 WHERE fingerprint=$2`, st, fp); err != nil {
			t.Fatalf("set state %s: %v", st, err)
		}
	}
	run2 := newScanRun(t, s, pipelineID, materialID, 2)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run2, "scan"), 0, nil); err != nil {
		t.Fatalf("reconcile r2 clean: %v", err)
	}

	page, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]bool{}
	for _, f := range page.Fixed {
		got[f.RuleID] = true
	}
	if !got["open-r"] || !got["accepted-r"] {
		t.Fatalf("open + accepted must appear as fixed, got %+v", page.Fixed)
	}
	if got["dismissed-r"] || got["fp-r"] {
		t.Fatalf("dismissed/false_positive must NOT resurface as fixed, got %+v", page.Fixed)
	}
	if page.FixedTotal != 2 {
		t.Fatalf("fixed total = %d, want 2 (open + accepted)", page.FixedTotal)
	}
}

// TestReplaceSecurityFindings_ReIngestPreservesIdentity guards the race: a
// re-ingest of the same fingerprint must not move first_seen_* and must not
// clobber a user's state — only last_seen_* + the snapshot advance.
func TestReplaceSecurityFindings_ReIngestPreservesIdentity(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	_, pipelineID, materialID := scanProject(t, pool, s, "sec-reingest", "scan")

	finding := store.FindingIn{Tool: "Trivy", RuleID: "CVE-1", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp1"}

	run1 := newScanRun(t, s, pipelineID, materialID, 1)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run1, "scan"), 0, []store.FindingIn{finding}); err != nil {
		t.Fatalf("reconcile r1: %v", err)
	}
	// A user dismisses it.
	if _, err := pool.Exec(ctx, `UPDATE security_finding_states SET state='dismissed', state_reason='noise', state_actor_email='me@x' WHERE fingerprint='fp1'`); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	var firstRun uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT first_seen_run_id FROM security_finding_states WHERE fingerprint='fp1'`).Scan(&firstRun); err != nil {
		t.Fatalf("read first_seen: %v", err)
	}

	// Re-ingest the same fingerprint in a later run.
	run2 := newScanRun(t, s, pipelineID, materialID, 2)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run2, "scan"), 0, []store.FindingIn{finding}); err != nil {
		t.Fatalf("reconcile r2: %v", err)
	}

	var (
		gotFirst, gotLast    uuid.UUID
		state, reason, actor string
	)
	if err := pool.QueryRow(ctx, `
		SELECT first_seen_run_id, last_seen_run_id, state, state_reason, state_actor_email
		FROM security_finding_states WHERE fingerprint='fp1'`,
	).Scan(&gotFirst, &gotLast, &state, &reason, &actor); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if gotFirst != firstRun {
		t.Fatalf("first_seen_run_id moved on re-ingest: %s -> %s", firstRun, gotFirst)
	}
	if gotLast != run2 {
		t.Fatalf("last_seen_run_id should advance to run2, got %s", gotLast)
	}
	if state != "dismissed" || reason != "noise" || actor != "me@x" {
		t.Fatalf("re-ingest clobbered user state: state=%q reason=%q actor=%q", state, reason, actor)
	}
}

// TestSecurityFindingStates_Backfill validates the migration's backfill SQL:
// given v1-era findings with no identities, it reconstructs them with first/last
// seen from the occurrences' own runs and the latest-occurrence snapshot.
func TestSecurityFindingStates_Backfill(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	_, pipelineID, materialID := scanProject(t, pool, s, "sec-backfill", "scan")

	finding := store.FindingIn{Tool: "Trivy", RuleID: "CVE-1", Severity: "high", Level: "error", Message: "snap", LocationPath: "go.sum", LocationLine: 3, Fingerprint: "fp1"}
	run1 := newScanRun(t, s, pipelineID, materialID, 1)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run1, "scan"), 0, []store.FindingIn{finding}); err != nil {
		t.Fatalf("reconcile r1: %v", err)
	}
	run2 := newScanRun(t, s, pipelineID, materialID, 2)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run2, "scan"), 0, []store.FindingIn{finding}); err != nil {
		t.Fatalf("reconcile r2: %v", err)
	}

	// Simulate the pre-v2 state: occurrences + markers exist, identities don't.
	if _, err := pool.Exec(ctx, `DELETE FROM security_finding_states`); err != nil {
		t.Fatalf("clear identities: %v", err)
	}

	// Run the migration's backfill statement (mirrors 00064).
	if _, err := pool.Exec(ctx, backfillIdentitiesSQL); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM security_finding_states WHERE fingerprint='fp1'`,
	).Scan(&count); err != nil {
		t.Fatalf("count backfilled identity: %v", err)
	}
	if count != 1 {
		t.Fatalf("backfill must create exactly one identity, got %d", count)
	}
	var (
		gotFirst, gotLast uuid.UUID
		snapMsg, snapPath string
		snapLine          int
	)
	if err := pool.QueryRow(ctx, `
		SELECT first_seen_run_id, last_seen_run_id, last_message, last_location_path, last_location_line
		FROM security_finding_states WHERE fingerprint='fp1'`,
	).Scan(&gotFirst, &gotLast, &snapMsg, &snapPath, &snapLine); err != nil {
		t.Fatalf("read backfilled identity: %v", err)
	}
	if gotFirst != run1 {
		t.Fatalf("first_seen_run_id = %s, want run1 %s", gotFirst, run1)
	}
	if gotLast != run2 {
		t.Fatalf("last_seen_run_id = %s, want run2 %s", gotLast, run2)
	}
	if snapMsg != "snap" || snapPath != "go.sum" || snapLine != 3 {
		t.Fatalf("snapshot from latest occurrence wrong: msg=%q path=%q line=%d", snapMsg, snapPath, snapLine)
	}
}

// backfillIdentitiesSQL mirrors the INSERT…SELECT in migration 00064. Kept here
// so the backfill logic (window functions, first/last seen, deterministic
// snapshot) is exercised by a test; keep in sync with the migration.
const backfillIdentitiesSQL = `
INSERT INTO security_finding_states (
    project_id, pipeline_id, scanner_job, matrix_key, tool, fingerprint,
    first_seen_run_id, first_seen_at, last_seen_run_id, last_seen_at,
    last_rule_id, last_severity, last_level, last_message,
    last_location_path, last_location_line
)
WITH ranked AS (
    SELECT
        f.project_id, f.pipeline_id, f.job_name AS scanner_job, f.matrix_key,
        f.tool, f.fingerprint, f.run_id, f.created_at,
        f.rule_id, f.severity, f.level, f.message, f.location_path, f.location_line,
        ROW_NUMBER() OVER (
            PARTITION BY f.pipeline_id, f.job_name, f.matrix_key, f.tool, f.fingerprint
            ORDER BY r.counter DESC, f.created_at DESC, f.id DESC
        ) AS rn_last,
        ROW_NUMBER() OVER (
            PARTITION BY f.pipeline_id, f.job_name, f.matrix_key, f.tool, f.fingerprint
            ORDER BY r.counter ASC, f.created_at ASC, f.id ASC
        ) AS rn_first
    FROM security_findings f
    JOIN runs r ON r.id = f.run_id
)
SELECT
    last.project_id, last.pipeline_id, last.scanner_job, last.matrix_key,
    last.tool, last.fingerprint,
    first.run_id, first.created_at,
    last.run_id, last.created_at,
    last.rule_id, last.severity, last.level, last.message,
    last.location_path, last.location_line
FROM (SELECT * FROM ranked WHERE rn_last = 1) last
JOIN (SELECT * FROM ranked WHERE rn_first = 1) first
    ON  first.pipeline_id = last.pipeline_id
    AND first.scanner_job = last.scanner_job
    AND first.matrix_key  = last.matrix_key
    AND first.tool        = last.tool
    AND first.fingerprint = last.fingerprint
ON CONFLICT (pipeline_id, scanner_job, matrix_key, tool, fingerprint) DO NOTHING`
