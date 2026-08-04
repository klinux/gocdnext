package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GetProjectTrustSameRepoPRConfigBySlug reports whether the project opted into
// running a same-repo PR against its own `.gocdnext/` (head config) — #223. The
// column is NOT NULL DEFAULT false, so an existing project always yields a
// value; a missing project returns ErrProjectNotFound.
func (s *Store) GetProjectTrustSameRepoPRConfigBySlug(ctx context.Context, slug string) (bool, error) {
	enabled, err := s.q.GetProjectTrustSameRepoPRConfigBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrProjectNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: get project trust_same_repo_pr_config: %w", err)
	}
	return enabled, nil
}

// SetProjectTrustSameRepoPRConfigBySlug flips the per-project opt-in. Returns
// ErrProjectNotFound (not an opaque 500) when the slug matches no project —
// RowsAffected distinguishes "not found" from a no-op update. Raw Exec so the
// tag is available; the mutation is admin-gated + audited at the API edge.
func (s *Store) SetProjectTrustSameRepoPRConfigBySlug(ctx context.Context, slug string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE projects SET trust_same_repo_pr_config = $2 WHERE slug = $1`,
		slug, enabled)
	if err != nil {
		return fmt.Errorf("store: set project trust_same_repo_pr_config: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProjectNotFound
	}
	return nil
}
