package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestPullRequestLifecycle_UpsertAnyOrder(t *testing.T) {
	s := store.New(dbtest.SetupPool(t))
	ctx := context.Background()

	opened := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	approved := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	merged := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)

	// Events arrive out of order: approval first (row created), then opened,
	// then merged.
	if err := s.RecordPullRequestApproved(ctx, "github", "acme/web", 42, approved); err != nil {
		t.Fatalf("approved: %v", err)
	}
	if err := s.RecordPullRequestOpened(ctx, "github", "acme/web", 42, opened, "Add feature", "dev", "feat", "main", "headsha"); err != nil {
		t.Fatalf("opened: %v", err)
	}
	if err := s.RecordPullRequestMerged(ctx, "github", "acme/web", 42, merged, "mergesha"); err != nil {
		t.Fatalf("merged: %v", err)
	}

	pr, err := s.PullRequest(ctx, "github", "acme/web", 42)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !pr.OpenedAt.Equal(opened) || !pr.ApprovedAt.Equal(approved) || !pr.MergedAt.Equal(merged) {
		t.Fatalf("timestamps = opened %v approved %v merged %v", pr.OpenedAt, pr.ApprovedAt, pr.MergedAt)
	}
	if pr.MergeSHA != "mergesha" || pr.HeadSHA != "headsha" {
		t.Errorf("sha = merge %q head %q", pr.MergeSHA, pr.HeadSHA)
	}
}

func TestPullRequestLifecycle_KeepsEarliest(t *testing.T) {
	s := store.New(dbtest.SetupPool(t))
	ctx := context.Background()

	first := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	later := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// opened_at keeps its first value across a later synchronize event.
	_ = s.RecordPullRequestOpened(ctx, "github", "acme/web", 7, first, "t", "a", "feat", "main", "sha1")
	_ = s.RecordPullRequestOpened(ctx, "github", "acme/web", 7, later, "t2", "a", "feat", "main", "sha2")

	// approved_at keeps the first approval, ignores a later one.
	_ = s.RecordPullRequestApproved(ctx, "github", "acme/web", 7, first)
	_ = s.RecordPullRequestApproved(ctx, "github", "acme/web", 7, later)

	pr, err := s.PullRequest(ctx, "github", "acme/web", 7)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !pr.OpenedAt.Equal(first) {
		t.Errorf("opened_at = %v, want first %v", pr.OpenedAt, first)
	}
	if !pr.ApprovedAt.Equal(first) {
		t.Errorf("approved_at = %v, want first %v", pr.ApprovedAt, first)
	}
	if pr.HeadSHA != "sha2" {
		t.Errorf("head_sha = %q, want latest sha2", pr.HeadSHA) // non-timestamp fields follow latest
	}
}
