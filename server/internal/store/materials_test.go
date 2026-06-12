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

// seedThreePipelinesWithURL plants three pipelines that all watch the same
// repo URL. Two declare `events: [push]` and one declares `events: [push,
// tag]` — the URL-lookup result is "all three" and the caller's Events
// filter narrows to one for a tag-push fan-out.
func seedThreePipelinesWithURL(t *testing.T, pool *pgxpool.Pool, url string) (idA, idB, idC uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()

	pipelines := []*domain.Pipeline{
		{
			Name:   "ci",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: domain.GitFingerprint(url, "main"), AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{
				Name: "c", Stage: "build", Image: "alpine",
				Tasks: []domain.Task{{Script: "true"}},
			}},
		},
		{
			Name:   "ci-dev",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: domain.GitFingerprint(url, "dev"), AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "dev", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{
				Name: "c", Stage: "build", Image: "alpine",
				Tasks: []domain.Task{{Script: "true"}},
			}},
		},
		{
			Name:   "release",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				// release watches tags — branch is empty / placeholder
				// because the webhook routing is URL-only for tag
				// events. Use a placeholder so the fingerprint stays
				// unique vs the other pipelines on the same URL.
				Type: domain.MaterialGit, Fingerprint: domain.GitFingerprint(url, "@tags"), AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push", "tag"}},
			}},
			Jobs: []domain.Job{{
				Name: "c", Stage: "build", Image: "alpine",
				Tasks: []domain.Task{{Script: "true"}},
			}},
		},
	}

	out, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "p-url", Name: "P URL", Pipelines: pipelines})
	if err != nil {
		t.Fatalf("ApplyProject: %v", err)
	}
	for _, p := range out.Pipelines {
		switch p.Name {
		case "ci":
			idA = p.PipelineID
		case "ci-dev":
			idB = p.PipelineID
		case "release":
			idC = p.PipelineID
		}
	}
	return idA, idB, idC
}

func TestFindMaterialsByCloneURL_MatchesAllPipelinesOnSameRepo(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	url := "https://github.com/org/url-lookup"
	idA, idB, idC := seedThreePipelinesWithURL(t, pool, url)

	mats, err := s.FindMaterialsByCloneURL(context.Background(), url)
	if err != nil {
		t.Fatalf("FindMaterialsByCloneURL: %v", err)
	}
	if len(mats) != 3 {
		t.Fatalf("matches = %d, want 3", len(mats))
	}
	// Assertion: every seeded pipeline shows up, regardless of branch.
	seen := map[uuid.UUID]bool{}
	for _, m := range mats {
		seen[m.PipelineID] = true
	}
	for _, want := range []uuid.UUID{idA, idB, idC} {
		if !seen[want] {
			t.Errorf("missing pipeline %s", want)
		}
	}
}

func TestFindMaterialsByCloneURL_NormalisesURLVariations(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	const repo = "https://github.com/org/normalize-me"
	_, _, _ = seedThreePipelinesWithURL(t, pool, repo)

	// Same canonical repo, different operator-typed forms. Each
	// should resolve to the same three materials.
	variants := []string{
		"https://github.com/org/normalize-me",
		"https://github.com/org/normalize-me.git",
		"http://github.com/org/normalize-me",
		"git@github.com:org/normalize-me.git",
	}
	for _, v := range variants {
		mats, err := s.FindMaterialsByCloneURL(context.Background(), v)
		if err != nil {
			t.Errorf("lookup %q: %v", v, err)
			continue
		}
		if len(mats) != 3 {
			t.Errorf("variant %q matched %d, want 3", v, len(mats))
		}
	}
}

func TestFindMaterialsByCloneURL_NoMatchOnDifferentRepo(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	_, _, _ = seedThreePipelinesWithURL(t, pool, "https://github.com/org/the-one")

	mats, err := s.FindMaterialsByCloneURL(context.Background(), "https://github.com/org/some-other-repo")
	if err != nil {
		t.Fatalf("FindMaterialsByCloneURL: %v", err)
	}
	if len(mats) != 0 {
		t.Errorf("matches = %d, want 0 — non-matching repo should not bleed into the result", len(mats))
	}
}

func TestFindMaterialsByCloneURL_EmptyDB(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	mats, err := s.FindMaterialsByCloneURL(context.Background(), "https://github.com/org/anything")
	if err != nil {
		t.Fatalf("FindMaterialsByCloneURL on empty DB: %v", err)
	}
	if len(mats) != 0 {
		t.Errorf("matches = %d, want 0", len(mats))
	}
}
