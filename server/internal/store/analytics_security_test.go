package store_test

import (
	"context"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// groupByName finds a rollup group by its label value.
func groupByName(rep store.SecurityRollupReport, name string) (store.SecurityRollupGroup, bool) {
	for _, g := range rep.Groups {
		if g.Group == name {
			return g, true
		}
	}
	return store.SecurityRollupGroup{}, false
}

// TestSecurityRollup covers the org/label rollup: identity counts (not
// occurrences), clean groups showing 0 (not dropped), has_scans for the
// never-scanned distinction, and dismissed/false_positive excluded with accepted
// counted separately.
func TestSecurityRollup(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// Project A (team=payments): one critical, one high, one dismissed, one accepted.
	projA, pipeA, matA := scanProject(t, pool, s, "rollup-a", "scan")
	if err := s.ReplaceProjectLabels(ctx, projA, []store.ProjectLabel{{Key: "team", Value: "payments"}}); err != nil {
		t.Fatalf("labels a: %v", err)
	}
	runA := newScanRun(t, s, pipeA, matA, 1)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, runA, "scan"), 0, []store.FindingIn{
		{Tool: "T", RuleID: "crit", Severity: "critical", Level: "error", Message: "m", Fingerprint: "fp-crit"},
		{Tool: "T", RuleID: "high", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-high"},
		{Tool: "T", RuleID: "dis", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-dis"},
		{Tool: "T", RuleID: "acc", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-acc"},
	}); err != nil {
		t.Fatalf("reconcile a: %v", err)
	}
	for fp, st := range map[string]string{"fp-dis": "dismissed", "fp-acc": "accepted"} {
		if _, err := pool.Exec(ctx, `UPDATE security_finding_states SET state=$1 WHERE fingerprint=$2`, st, fp); err != nil {
			t.Fatalf("state %s: %v", st, err)
		}
	}

	// Project B (team=payments): scanned clean (0 findings) — group stays scanned.
	projB, pipeB, matB := scanProject(t, pool, s, "rollup-b", "scan")
	if err := s.ReplaceProjectLabels(ctx, projB, []store.ProjectLabel{{Key: "team", Value: "payments"}}); err != nil {
		t.Fatalf("labels b: %v", err)
	}
	runB := newScanRun(t, s, pipeB, matB, 1)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, runB, "scan"), 0, nil); err != nil {
		t.Fatalf("reconcile b clean: %v", err)
	}

	// Project C (team=storefront): never scanned.
	projC, _, _ := scanProject(t, pool, s, "rollup-c", "scan")
	if err := s.ReplaceProjectLabels(ctx, projC, []store.ProjectLabel{{Key: "team", Value: "storefront"}}); err != nil {
		t.Fatalf("labels c: %v", err)
	}

	rep, err := s.SecurityRollup(ctx, "team")
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if rep.Key != "team" || len(rep.Groups) != 2 {
		t.Fatalf("groups = %+v", rep.Groups)
	}

	pay, ok := groupByName(rep, "payments")
	if !ok {
		t.Fatalf("payments group missing: %+v", rep.Groups)
	}
	if pay.Critical != 1 || pay.High != 1 || pay.TotalOpen != 2 {
		t.Fatalf("payments open = %+v (want 1 crit, 1 high, 2 total — dismissed excluded)", pay)
	}
	if pay.Accepted != 1 {
		t.Fatalf("payments accepted = %d, want 1 (counted separately)", pay.Accepted)
	}
	if !pay.HasScans {
		t.Fatalf("payments should be scanned")
	}

	sf, ok := groupByName(rep, "storefront")
	if !ok {
		t.Fatalf("storefront group must appear even with no scans: %+v", rep.Groups)
	}
	if sf.TotalOpen != 0 || sf.HasScans {
		t.Fatalf("storefront = %+v (want 0 open, has_scans=false)", sf)
	}

	if rep.OrgCritical != 1 || rep.OrgTotalOpen != 2 || rep.OrgAccepted != 1 {
		t.Fatalf("org totals = crit %d, open %d, accepted %d", rep.OrgCritical, rep.OrgTotalOpen, rep.OrgAccepted)
	}
}

// TestSecurityRollup_IdentityNotOccurrence guards that the rollup counts one
// identity per fingerprint even when a scan emits duplicate occurrences.
func TestSecurityRollup_IdentityNotOccurrence(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	proj, pipe, mat := scanProject(t, pool, s, "rollup-dup", "scan")
	if err := s.ReplaceProjectLabels(ctx, proj, []store.ProjectLabel{{Key: "team", Value: "x"}}); err != nil {
		t.Fatalf("labels: %v", err)
	}
	dup := store.FindingIn{Tool: "T", RuleID: "r", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp-dup"}
	run := newScanRun(t, s, pipe, mat, 1)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run, "scan"), 0, []store.FindingIn{dup, dup}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	rep, err := s.SecurityRollup(ctx, "team")
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	g, ok := groupByName(rep, "x")
	if !ok || g.High != 1 || g.TotalOpen != 1 {
		t.Fatalf("duplicate occurrences must count as 1 identity, got %+v", g)
	}
}

// TestReplaceSecurityFindings_WorstSeverityWins guards the in-scan dedupe: two
// occurrences of one fingerprint at medium+high snapshot the identity as high,
// regardless of order.
func TestReplaceSecurityFindings_WorstSeverityWins(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, pipe, mat := scanProject(t, pool, s, "worst-sev", "scan")
	run := newScanRun(t, s, pipe, mat, 1)
	// medium first, then high — worst (high) must win.
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run, "scan"), 0, []store.FindingIn{
		{Tool: "T", RuleID: "r", Severity: "medium", Level: "warning", Message: "m", Fingerprint: "fp1"},
		{Tool: "T", RuleID: "r", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp1"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var sev string
	if err := pool.QueryRow(ctx,
		`SELECT last_severity FROM security_finding_states WHERE fingerprint='fp1'`).Scan(&sev); err != nil {
		t.Fatalf("read: %v", err)
	}
	if sev != "high" {
		t.Fatalf("worst-severity-wins: identity severity = %q, want high", sev)
	}

	// And the reverse order (high first, then medium) still snapshots high.
	run2 := newScanRun(t, s, pipe, mat, 2)
	if err := s.ReplaceSecurityFindings(ctx, jobRunByName(t, pool, run2, "scan"), 0, []store.FindingIn{
		{Tool: "T", RuleID: "r", Severity: "high", Level: "error", Message: "m", Fingerprint: "fp1"},
		{Tool: "T", RuleID: "r", Severity: "medium", Level: "warning", Message: "m", Fingerprint: "fp1"},
	}); err != nil {
		t.Fatalf("reconcile r2: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT last_severity FROM security_finding_states WHERE fingerprint='fp1'`).Scan(&sev); err != nil {
		t.Fatalf("read r2: %v", err)
	}
	if sev != "high" {
		t.Fatalf("worst-severity-wins (reverse order): severity = %q, want high", sev)
	}
}
