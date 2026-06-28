package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// PullRequestLifecycle is the persisted lifecycle of one pull request — the
// timestamps DORA lead-time decomposition reads (#112).
type PullRequestLifecycle struct {
	Provider   string
	Repo       string
	Number     int64
	HeadSHA    string
	MergeSHA   string
	OpenedAt   time.Time
	ApprovedAt time.Time
	MergedAt   time.Time
}

func ts(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// RecordPullRequestOpened upserts the opened-side fields of a PR. opened_at
// keeps its first value across repeated events.
func (s *Store) RecordPullRequestOpened(ctx context.Context, provider, repo string, number int, openedAt time.Time, title, author, headRef, baseRef, headSHA string) error {
	if err := s.q.UpsertPullRequestOpened(ctx, db.UpsertPullRequestOpenedParams{
		Provider: provider,
		Repo:     repo,
		Number:   int64(number),
		Title:    title,
		Author:   author,
		HeadRef:  headRef,
		BaseRef:  baseRef,
		HeadSha:  headSHA,
		OpenedAt: ts(openedAt),
	}); err != nil {
		return fmt.Errorf("store: record pr opened: %w", err)
	}
	return nil
}

// RecordPullRequestApproved records the first approving review's timestamp.
func (s *Store) RecordPullRequestApproved(ctx context.Context, provider, repo string, number int, approvedAt time.Time) error {
	if err := s.q.MarkPullRequestApproved(ctx, db.MarkPullRequestApprovedParams{
		Provider:   provider,
		Repo:       repo,
		Number:     int64(number),
		ApprovedAt: ts(approvedAt),
	}); err != nil {
		return fmt.Errorf("store: record pr approved: %w", err)
	}
	return nil
}

// RecordPullRequestMerged records the merge timestamp + the commit that landed
// on the base branch.
func (s *Store) RecordPullRequestMerged(ctx context.Context, provider, repo string, number int, mergedAt time.Time, mergeSHA string) error {
	if err := s.q.MarkPullRequestMerged(ctx, db.MarkPullRequestMergedParams{
		Provider: provider,
		Repo:     repo,
		Number:   int64(number),
		MergeSha: mergeSHA,
		MergedAt: ts(mergedAt),
		ClosedAt: ts(mergedAt),
	}); err != nil {
		return fmt.Errorf("store: record pr merged: %w", err)
	}
	return nil
}

// PullRequest reads one PR's lifecycle (for tests / phase-2 correlation).
func (s *Store) PullRequest(ctx context.Context, provider, repo string, number int) (PullRequestLifecycle, error) {
	row, err := s.q.GetPullRequest(ctx, db.GetPullRequestParams{
		Provider: provider, Repo: repo, Number: int64(number),
	})
	if err != nil {
		return PullRequestLifecycle{}, fmt.Errorf("store: get pr: %w", err)
	}
	pr := PullRequestLifecycle{
		Provider: row.Provider, Repo: row.Repo, Number: row.Number,
		HeadSHA: row.HeadSha, MergeSHA: row.MergeSha,
	}
	if row.OpenedAt.Valid {
		pr.OpenedAt = row.OpenedAt.Time
	}
	if row.ApprovedAt.Valid {
		pr.ApprovedAt = row.ApprovedAt.Time
	}
	if row.MergedAt.Valid {
		pr.MergedAt = row.MergedAt.Time
	}
	return pr, nil
}
