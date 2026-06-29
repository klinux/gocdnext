package store_test

import (
	"context"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestComplianceCoverage(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	soc2, err := s.InsertComplianceFramework(ctx, store.FrameworkInput{Name: "SOC2"})
	if err != nil {
		t.Fatalf("framework soc2: %v", err)
	}
	iso, err := s.InsertComplianceFramework(ctx, store.FrameworkInput{Name: "ISO27001"})
	if err != nil {
		t.Fatalf("framework iso: %v", err)
	}

	mk := func(slug, value string, fws ...string) {
		t.Helper()
		res, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: slug, Name: slug})
		if err != nil {
			t.Fatalf("apply %s: %v", slug, err)
		}
		if err := s.ReplaceProjectLabels(ctx, res.ProjectID, []store.ProjectLabel{{Key: "team", Value: value}}); err != nil {
			t.Fatalf("labels %s: %v", slug, err)
		}
		if len(fws) > 0 {
			if err := s.SetProjectFrameworks(ctx, res.ProjectID, fws); err != nil {
				t.Fatalf("frameworks %s: %v", slug, err)
			}
		}
	}

	// payments: 2 projects — a bound to both frameworks, b to SOC2 only.
	mk("a", "payments", soc2.ID, iso.ID)
	mk("b", "payments", soc2.ID)
	// storefront: 1 project, no frameworks bound.
	mk("c", "storefront")

	rep, err := s.ComplianceCoverage(ctx, "team")
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if rep.Key != "team" || len(rep.Groups) != 2 {
		t.Fatalf("report = %+v", rep)
	}

	pay := rep.Groups[0]
	if pay.Group != "payments" || pay.ProjectsTotal != 2 {
		t.Fatalf("payments group = %+v", pay)
	}
	// Frameworks ordered by name: ISO27001 (1/2), SOC2 (2/2).
	if len(pay.Frameworks) != 2 ||
		pay.Frameworks[0].Framework != "ISO27001" || pay.Frameworks[0].Covered != 1 ||
		pay.Frameworks[1].Framework != "SOC2" || pay.Frameworks[1].Covered != 2 {
		t.Fatalf("payments frameworks = %+v", pay.Frameworks)
	}

	store := rep.Groups[1]
	if store.Group != "storefront" || store.ProjectsTotal != 1 || len(store.Frameworks) != 0 {
		t.Fatalf("storefront group = %+v (frameworks must be empty, not null)", store)
	}
}
