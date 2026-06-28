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

func TestPullRequestLifecycle_EarliestWinsOutOfOrder(t *testing.T) {
	s := store.New(dbtest.SetupPool(t))
	ctx := context.Background()

	earlier := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	later := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// Deliveries arrive OUT OF ORDER: the later timestamp lands first, the
	// earlier one second. opened_at + approved_at must end on the EARLIEST
	// (LEAST), not the first-received — otherwise phase-2 Review timing skews.
	_ = s.RecordPullRequestOpened(ctx, "github", "acme/web", 7, later, "t", "a", "feat", "main", "shaLater")
	_ = s.RecordPullRequestOpened(ctx, "github", "acme/web", 7, earlier, "t2", "a", "feat", "main", "shaEarlier")

	_ = s.RecordPullRequestApproved(ctx, "github", "acme/web", 7, later)
	_ = s.RecordPullRequestApproved(ctx, "github", "acme/web", 7, earlier)

	// first_commit_at: later fetched first (e.g. opened), earlier on a retry.
	_ = s.RecordPullRequestFirstCommit(ctx, "github", "acme/web", 7, later)
	_ = s.RecordPullRequestFirstCommit(ctx, "github", "acme/web", 7, earlier)

	pr, err := s.PullRequest(ctx, "github", "acme/web", 7)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !pr.OpenedAt.Equal(earlier) {
		t.Errorf("opened_at = %v, want earliest %v", pr.OpenedAt, earlier)
	}
	if !pr.ApprovedAt.Equal(earlier) {
		t.Errorf("approved_at = %v, want earliest %v", pr.ApprovedAt, earlier)
	}
	if !pr.FirstCommitAt.Equal(earlier) {
		t.Errorf("first_commit_at = %v, want earliest %v", pr.FirstCommitAt, earlier)
	}
	// Non-timestamp fields still follow the latest upsert.
	if pr.HeadSHA != "shaEarlier" {
		t.Errorf("head_sha = %q, want latest-upsert shaEarlier", pr.HeadSHA)
	}
}
