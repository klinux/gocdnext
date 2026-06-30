package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// mainlineRun creates a webhook (mainline) run on a given branch.
func mainlineRun(t *testing.T, s *store.Store, pipelineID, materialID uuid.UUID, mod int64, branch string) uuid.UUID {
	t.Helper()
	res, err := s.CreateRunFromModification(context.Background(), store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID, ModificationID: mod,
		Revision: "r", Branch: branch, Provider: "github", Delivery: "t", TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("mainline run %d: %v", mod, err)
	}
	return res.RunID
}

// prRun creates a pull_request run with a base ref.
func prRun(t *testing.T, s *store.Store, pipelineID, materialID uuid.UUID, mod int64, baseRef, head string) uuid.UUID {
	t.Helper()
	res, err := s.CreateRunFromModification(context.Background(), store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID, ModificationID: mod,
		Revision: "r", Branch: head, Provider: "github", Delivery: "t", TriggeredBy: "system:test",
		Cause:       "pull_request",
		CauseDetail: json.RawMessage(`{"pr_base_ref":"` + baseRef + `"}`),
	})
	if err != nil {
		t.Fatalf("pr run %d: %v", mod, err)
	}
	return res.RunID
}

func finding(tool, rule, sev, fp string) store.FindingIn {
	return store.FindingIn{Tool: tool, RuleID: rule, Severity: sev, Level: "error", Message: "m", Fingerprint: fp}
}

func TestRunSecuritySummary_NewVsBase(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	_, pipe, mat := scanProject(t, pool, s, "rsum-new", "scan")

	base := mainlineRun(t, s, pipe, mat, 1, "main")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, base, "scan"), 0, []store.FindingIn{
		finding("T", "old", "high", "fp-old"),
	}); err != nil {
		t.Fatalf("base reconcile: %v", err)
	}
	pr := prRun(t, s, pipe, mat, 2, "main", "feature")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, pr, "scan"), 0, []store.FindingIn{
		finding("T", "old", "high", "fp-old"),
		finding("T", "new", "critical", "fp-new"),
	}); err != nil {
		t.Fatalf("pr reconcile: %v", err)
	}

	sec, err := s.RunSecuritySummary(ctx, pr)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if !sec.HasScans || !sec.DeltaAvailable || sec.UnbaselinedSeries != 0 {
		t.Fatalf("flags = %+v", sec)
	}
	if len(sec.NewInChange) != 1 || sec.NewInChange[0].RuleID != "new" {
		t.Fatalf("new_in_change must be just the introduced finding, got %+v", sec.NewInChange)
	}
	if sec.Critical != 1 || sec.High != 1 || sec.OpenTotal != 2 {
		t.Fatalf("open counts = %+v", sec)
	}
}

func TestRunSecuritySummary_EmptyBaseIsUnbaselinedNotNew(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	_, pipe, mat := scanProject(t, pool, s, "rsum-empty", "scan")

	// PR with findings but NO mainline base scan exists.
	pr := prRun(t, s, pipe, mat, 1, "main", "feature")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, pr, "scan"), 0, []store.FindingIn{
		finding("T", "x", "high", "fp-x"),
	}); err != nil {
		t.Fatalf("pr reconcile: %v", err)
	}
	sec, err := s.RunSecuritySummary(ctx, pr)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sec.DeltaAvailable {
		t.Fatalf("no base scan → delta_available must be false")
	}
	if sec.UnbaselinedSeries != 1 {
		t.Fatalf("unbaselined_series = %d, want 1 (read empty base ≠ 0 new)", sec.UnbaselinedSeries)
	}
	if len(sec.NewInChange) != 0 {
		t.Fatalf("no comparable base → nothing is 'new', got %+v", sec.NewInChange)
	}
}

func TestRunSecuritySummary_NotPRNoDelta(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	_, pipe, mat := scanProject(t, pool, s, "rsum-notpr", "scan")

	run := mainlineRun(t, s, pipe, mat, 1, "main")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run, "scan"), 0, []store.FindingIn{
		finding("T", "x", "high", "fp-x"),
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	sec, err := s.RunSecuritySummary(ctx, run)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sec.DeltaAvailable || sec.UnbaselinedSeries != 0 || len(sec.NewInChange) != 0 {
		t.Fatalf("non-PR run → no delta, got %+v", sec)
	}
	if sec.High != 1 || sec.OpenTotal != 1 {
		t.Fatalf("open counts still returned, got %+v", sec)
	}
}

func TestRunSecuritySummary_CleanBaselineIsComparable(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	_, pipe, mat := scanProject(t, pool, s, "rsum-clean", "scan")

	base := mainlineRun(t, s, pipe, mat, 1, "main")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, base, "scan"), 0, nil); err != nil {
		t.Fatalf("base clean: %v", err)
	}
	pr := prRun(t, s, pipe, mat, 2, "main", "feature")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, pr, "scan"), 0, []store.FindingIn{
		finding("T", "new", "high", "fp-new"),
	}); err != nil {
		t.Fatalf("pr reconcile: %v", err)
	}
	sec, err := s.RunSecuritySummary(ctx, pr)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if !sec.DeltaAvailable || sec.UnbaselinedSeries != 0 {
		t.Fatalf("clean base series is comparable, got %+v", sec)
	}
	if len(sec.NewInChange) != 1 {
		t.Fatalf("finding vs a clean base IS new, got %+v", sec.NewInChange)
	}
}

func TestRunSecuritySummary_PerScannerBaseline(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	_, pipe, mat := scanProject(t, pool, s, "rsum-perscanner", "trivy", "semgrep")

	// Base run 1: trivy clean + semgrep has a finding.
	b1 := mainlineRun(t, s, pipe, mat, 1, "main")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, b1, "trivy"), 0, nil); err != nil {
		t.Fatalf("b1 trivy: %v", err)
	}
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, b1, "semgrep"), 0, []store.FindingIn{
		finding("Semgrep", "r-xss", "high", "fp-semgrep"),
	}); err != nil {
		t.Fatalf("b1 semgrep: %v", err)
	}
	// Base run 2 (newer): only trivy reconciles; semgrep not scanned this run.
	b2 := mainlineRun(t, s, pipe, mat, 2, "main")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, b2, "trivy"), 0, nil); err != nil {
		t.Fatalf("b2 trivy: %v", err)
	}
	// PR run: semgrep reports the SAME finding — must NOT be new (semgrep's
	// latest base scan is b1, which had it). The per-scanner baseline ignores
	// b2 having no semgrep.
	pr := prRun(t, s, pipe, mat, 3, "main", "feature")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, pr, "semgrep"), 0, []store.FindingIn{
		finding("Semgrep", "r-xss", "high", "fp-semgrep"),
	}); err != nil {
		t.Fatalf("pr semgrep: %v", err)
	}
	sec, err := s.RunSecuritySummary(ctx, pr)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(sec.NewInChange) != 0 {
		t.Fatalf("semgrep finding present on base latest semgrep scan must NOT be new, got %+v", sec.NewInChange)
	}
}

func TestRunSecuritySummary_BranchAware(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	_, pipe, mat := scanProject(t, pool, s, "rsum-branch", "scan")

	// main baseline has fp-main; a NEWER release/x mainline has fp-rel.
	bmain := mainlineRun(t, s, pipe, mat, 1, "main")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, bmain, "scan"), 0, []store.FindingIn{
		finding("T", "main-rule", "high", "fp-main"),
	}); err != nil {
		t.Fatalf("bmain: %v", err)
	}
	brel := mainlineRun(t, s, pipe, mat, 2, "release/x")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, brel, "scan"), 0, []store.FindingIn{
		finding("T", "rel-rule", "high", "fp-rel"),
	}); err != nil {
		t.Fatalf("brel: %v", err)
	}
	// PR to main carries both fp-main and fp-rel. Baseline must be main only:
	// fp-main not new, fp-rel IS new (release/x must NOT serve as base for main).
	pr := prRun(t, s, pipe, mat, 3, "main", "feature")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, pr, "scan"), 0, []store.FindingIn{
		finding("T", "main-rule", "high", "fp-main"),
		finding("T", "rel-rule", "high", "fp-rel"),
	}); err != nil {
		t.Fatalf("pr: %v", err)
	}
	sec, err := s.RunSecuritySummary(ctx, pr)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(sec.NewInChange) != 1 || sec.NewInChange[0].RuleID != "rel-rule" {
		t.Fatalf("only the non-main finding is new vs main base, got %+v", sec.NewInChange)
	}
}

func TestRunSecuritySummary_HasScansCleanVsAbsent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	_, pipe, mat := scanProject(t, pool, s, "rsum-hasscans", "scan")

	// Never scanned.
	never := mainlineRun(t, s, pipe, mat, 1, "main")
	sec, err := s.RunSecuritySummary(ctx, never)
	if err != nil {
		t.Fatalf("never: %v", err)
	}
	if sec.HasScans {
		t.Fatalf("an unscanned run must report has_scans=false")
	}
	// Reconciled clean.
	clean := mainlineRun(t, s, pipe, mat, 2, "main")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, clean, "scan"), 0, nil); err != nil {
		t.Fatalf("clean: %v", err)
	}
	sec, err = s.RunSecuritySummary(ctx, clean)
	if err != nil {
		t.Fatalf("clean summary: %v", err)
	}
	if !sec.HasScans || sec.OpenTotal != 0 {
		t.Fatalf("a clean reconciled run is has_scans=true, 0 open, got %+v", sec)
	}
}

func TestRunSecuritySummary_DuplicateAndFalsePositive(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	_, pipe, mat := scanProject(t, pool, s, "rsum-dupfp", "scan")

	// A clean base scan exists so the PR's series is comparable.
	base := mainlineRun(t, s, pipe, mat, 1, "main")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, base, "scan"), 0, nil); err != nil {
		t.Fatalf("base clean: %v", err)
	}
	pr := prRun(t, s, pipe, mat, 2, "main", "feature")
	dup := finding("T", "dup", "high", "fp-dup")
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, pr, "scan"), 0, []store.FindingIn{dup, dup}); err != nil {
		t.Fatalf("pr: %v", err)
	}
	sec, err := s.RunSecuritySummary(ctx, pr)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(sec.NewInChange) != 1 {
		t.Fatalf("duplicate occurrences → 1 new identity, got %+v", sec.NewInChange)
	}
	// Mark it false_positive → drops out of new + open.
	if _, err := pool.Exec(ctx, `UPDATE security_finding_states SET state='false_positive' WHERE fingerprint='fp-dup'`); err != nil {
		t.Fatalf("fp: %v", err)
	}
	sec, err = s.RunSecuritySummary(ctx, pr)
	if err != nil {
		t.Fatalf("summary2: %v", err)
	}
	if len(sec.NewInChange) != 0 || sec.OpenTotal != 0 {
		t.Fatalf("false_positive must not count as new/open, got %+v", sec)
	}
}
