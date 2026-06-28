package store_test

import (
	"context"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestProjectLabels_ReplaceListAndDedupe(t *testing.T) {
	s := store.New(dbtest.SetupPool(t))
	ctx := context.Background()

	a, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "a", Name: "a"})
	if err != nil {
		t.Fatalf("seed a: %v", err)
	}
	b, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "b", Name: "b"})
	if err != nil {
		t.Fatalf("seed b: %v", err)
	}

	// Set with a duplicate pair — must be deduped.
	if err := s.ReplaceProjectLabels(ctx, a.ProjectID, []store.ProjectLabel{
		{Key: "team", Value: "payments"},
		{Key: "tier", Value: "critical"},
		{Key: "team", Value: "payments"},
	}); err != nil {
		t.Fatalf("replace a: %v", err)
	}
	if err := s.ReplaceProjectLabels(ctx, b.ProjectID, []store.ProjectLabel{
		{Key: "team", Value: "web"},
	}); err != nil {
		t.Fatalf("replace b: %v", err)
	}

	got, err := s.ProjectLabels(ctx, a.ProjectID)
	if err != nil {
		t.Fatalf("list a: %v", err)
	}
	// Ordered by key,value; dedup → 2 not 3.
	if len(got) != 2 || got[0] != (store.ProjectLabel{Key: "team", Value: "payments"}) ||
		got[1] != (store.ProjectLabel{Key: "tier", Value: "critical"}) {
		t.Fatalf("labels a = %+v", got)
	}

	all, err := s.AllProjectLabels(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all[a.ProjectID]) != 2 || len(all[b.ProjectID]) != 1 {
		t.Fatalf("all labels = %+v", all)
	}

	// Replace with empty clears the set.
	if err := s.ReplaceProjectLabels(ctx, a.ProjectID, nil); err != nil {
		t.Fatalf("clear a: %v", err)
	}
	if got, _ := s.ProjectLabels(ctx, a.ProjectID); len(got) != 0 {
		t.Fatalf("expected cleared, got %+v", got)
	}
}

func TestProjectLabels_OnProjectSummaryAndDetail(t *testing.T) {
	s := store.New(dbtest.SetupPool(t))
	ctx := context.Background()
	p, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "svc", Name: "svc"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.ReplaceProjectLabels(ctx, p.ProjectID, []store.ProjectLabel{{Key: "team", Value: "x"}}); err != nil {
		t.Fatalf("labels: %v", err)
	}

	// Detail carries labels.
	d, err := s.GetProjectDetail(ctx, "svc", 1)
	if err != nil || len(d.Project.Labels) != 1 || d.Project.Labels[0].Key != "team" {
		t.Fatalf("detail labels = %+v err=%v", d.Project.Labels, err)
	}

	// List page carries labels (no N+1), defaulting to [] not nil for the
	// unlabeled case.
	list, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, ps := range list {
		if ps.Slug == "svc" {
			found = true
			if len(ps.Labels) != 1 || ps.Labels[0].Value != "x" {
				t.Fatalf("summary labels = %+v", ps.Labels)
			}
		}
	}
	if !found {
		t.Fatal("project not in list")
	}
}
