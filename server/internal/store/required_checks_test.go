package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedRequiredChecksProject applies a project with one PR-firing pipeline
// ("build", events push+pull_request) and one push-only pipeline ("nightly").
// Returns the store + ctx.
func seedRequiredChecksProject(t *testing.T, slug string) (*store.Store, context.Context) {
	t.Helper()
	s := store.New(dbtest.SetupPool(t))
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	url := "https://github.com/acme/" + slug
	fp := store.FingerprintFor(url, "main")
	prPipe := &domain.Pipeline{
		Name:   "build",
		Stages: []string{"ci"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push", "pull_request"}},
		}},
		Jobs: []domain.Job{{Name: "compile", Stage: "ci", Tasks: []domain.Task{{Script: "make"}}}},
	}
	pushPipe := &domain.Pipeline{
		Name:   "nightly",
		Stages: []string{"ci"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
		}},
		Jobs: []domain.Job{{Name: "sweep", Stage: "ci", Tasks: []domain.Task{{Script: "make sweep"}}}},
	}
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: url, DefaultBranch: "main"},
		Pipelines: []*domain.Pipeline{prPipe, pushPipe},
	}); err != nil {
		t.Fatalf("apply project: %v", err)
	}
	return s, ctx
}

func TestListPRFiringPipelineNames(t *testing.T) {
	s, ctx := seedRequiredChecksProject(t, "svc-a")
	names, err := s.ListPRFiringPipelineNames(ctx, "svc-a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Only the pull_request material qualifies; the push-only one does not.
	if len(names) != 1 || names[0] != "build" {
		t.Fatalf("PR-firing pipelines = %v, want [build]", names)
	}
}

// Eligibility is narrow: a PR-firing material must ALSO target the default
// branch and carry no path filter, else its check can't be relied on to post
// for every PR to the default branch (deadlock risk).
func TestListPRFiringPipelineNamesExcludesUnsafe(t *testing.T) {
	s := store.New(dbtest.SetupPool(t))
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()
	url := "https://github.com/acme/edge"

	mk := func(name, mURL, branch string, events, paths []string) *domain.Pipeline {
		return &domain.Pipeline{
			Name: name, Stages: []string{"ci"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: store.FingerprintFor(mURL, branch), AutoUpdate: true,
				Git: &domain.GitMaterial{URL: mURL, Branch: branch, Events: events, Paths: paths},
			}},
			Jobs: []domain.Job{{Name: "j", Stage: "ci", Tasks: []domain.Task{{Script: "x"}}}},
		}
	}
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "edge", Name: "edge",
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: url, DefaultBranch: "main"},
		Pipelines: []*domain.Pipeline{
			mk("on-default", url, "main", []string{"pull_request"}, nil),
			mk("on-release", url, "release", []string{"pull_request"}, nil),               // wrong branch
			mk("path-scoped", url, "main", []string{"pull_request"}, []string{"docs/**"}), // path filter
			mk("other-repo", "https://github.com/acme/elsewhere", "main", []string{"pull_request"}, nil),
		},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	names, err := s.ListPRFiringPipelineNames(ctx, "edge")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 1 || names[0] != "on-default" {
		t.Fatalf("eligible = %v, want [on-default] (branch, path, other-repo filtered out)", names)
	}
}

func TestSetProjectRequiredChecksRoundTrip(t *testing.T) {
	s, ctx := seedRequiredChecksProject(t, "svc-b")

	// Not configured yet.
	got, err := s.GetProjectRequiredChecks(ctx, "svc-b")
	if err != nil || got != nil {
		t.Fatalf("expected nil config, got %+v err=%v", got, err)
	}

	if err := s.SetProjectRequiredChecks(ctx, "svc-b", []string{"build"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err = s.GetProjectRequiredChecks(ctx, "svc-b")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || len(got.Pipelines) != 1 || got.Pipelines[0] != "build" {
		t.Fatalf("roundtrip = %+v, want pipelines [build]", got)
	}
	// A fresh set is pending sync: no ruleset id, no synced timestamp.
	if got.RulesetID != nil || got.SyncedAt != nil {
		t.Fatalf("fresh config should be pending sync, got %+v", got)
	}

	// Clearing keeps a nil-list config that the reconciler can act on.
	if err := s.SetProjectRequiredChecks(ctx, "svc-b", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err = s.GetProjectRequiredChecks(ctx, "svc-b")
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if got == nil || len(got.Pipelines) != 0 {
		t.Fatalf("after clear = %+v, want empty pipelines", got)
	}
}

func TestSetProjectRequiredChecksRejectsUnknownPipeline(t *testing.T) {
	s, ctx := seedRequiredChecksProject(t, "svc-c")
	err := s.SetProjectRequiredChecks(ctx, "svc-c", []string{"ghost"})
	if !errors.Is(err, store.ErrRequiredCheckUnreportable) {
		t.Fatalf("expected ErrRequiredCheckUnreportable for unknown pipeline, got %v", err)
	}
}

func TestSetProjectRequiredChecksRejectsNonPRFiring(t *testing.T) {
	s, ctx := seedRequiredChecksProject(t, "svc-d")
	// "nightly" exists but is push-only → its check never posts on a PR.
	err := s.SetProjectRequiredChecks(ctx, "svc-d", []string{"nightly"})
	if !errors.Is(err, store.ErrRequiredCheckUnreportable) {
		t.Fatalf("expected rejection of push-only pipeline, got %v", err)
	}
}

func TestSetProjectRequiredChecksRejectsCheckRunOnlyMode(t *testing.T) {
	s, ctx := seedRequiredChecksProject(t, "svc-e")
	if err := s.SetProjectCheckReportingBySlug(ctx, "svc-e", store.CheckReportingCheckRun); err != nil {
		t.Fatalf("set reporting mode: %v", err)
	}
	err := s.SetProjectRequiredChecks(ctx, "svc-e", []string{"build"})
	if !errors.Is(err, store.ErrRequiredCheckUnreportable) {
		t.Fatalf("expected rejection under check_run-only mode, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "commit-status") {
		t.Fatalf("error should explain the commit-status dependency, got %v", err)
	}
}

func TestRequiredChecksConfigValidateBounds(t *testing.T) {
	// Over the cap.
	big := make([]string, 51)
	for i := range big {
		big[i] = string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	if err := (&store.RequiredChecksConfig{Pipelines: big}).Validate(); err == nil {
		t.Fatal("expected bounds rejection for >50 pipelines")
	}
	// Duplicate.
	if err := (&store.RequiredChecksConfig{Pipelines: []string{"a", "a"}}).Validate(); err == nil {
		t.Fatal("expected duplicate rejection")
	}
	// Empty name.
	if err := (&store.RequiredChecksConfig{Pipelines: []string{""}}).Validate(); err == nil {
		t.Fatal("expected empty-name rejection")
	}
	// Nil config is valid.
	if err := (*store.RequiredChecksConfig)(nil).Validate(); err != nil {
		t.Fatalf("nil config should validate, got %v", err)
	}
}

func TestGetProjectRequiredChecksUnknownProject(t *testing.T) {
	s := store.New(dbtest.SetupPool(t))
	_, err := s.GetProjectRequiredChecks(context.Background(), "nope")
	if !errors.Is(err, store.ErrProjectNotFound) {
		t.Fatalf("expected ErrProjectNotFound, got %v", err)
	}
}
