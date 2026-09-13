package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// BadgeRun is the latest run that an anonymous badge token is allowed to see.
// The query only returns one when the project slug and badge token hash match.
type BadgeRun struct {
	ID           uuid.UUID
	Status       string
	PipelineName string
	CreatedAt    time.Time
}

// GetProjectBadgeEnabledBySlug reports whether anonymous badges are enabled for
// a project. It does not expose the stored token hash.
func (s *Store) GetProjectBadgeEnabledBySlug(ctx context.Context, slug string) (bool, error) {
	enabled, err := s.q.GetProjectBadgeEnabledBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrProjectNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: get project badge enabled: %w", err)
	}
	return enabled, nil
}

// SetProjectBadgeTokenHashBySlug enables or rotates the anonymous badge token.
// The caller owns token generation and hashing; the store never persists the
// plaintext token.
func (s *Store) SetProjectBadgeTokenHashBySlug(ctx context.Context, slug, tokenHash string) error {
	tag, err := s.q.SetProjectBadgeTokenHashBySlug(ctx, db.SetProjectBadgeTokenHashBySlugParams{
		Slug:           slug,
		BadgeTokenHash: tokenHash,
	})
	if err != nil {
		return fmt.Errorf("store: set project badge token hash: %w", err)
	}
	if tag == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// ClearProjectBadgeTokenBySlug disables anonymous badges for a project.
func (s *Store) ClearProjectBadgeTokenBySlug(ctx context.Context, slug string) error {
	tag, err := s.q.ClearProjectBadgeTokenBySlug(ctx, slug)
	if err != nil {
		return fmt.Errorf("store: clear project badge token: %w", err)
	}
	if tag == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// GetBadgeLatestRun returns the latest matching run for a valid badge token.
// A missing row covers all intentionally indistinguishable public cases:
// unknown project, disabled badge, invalid token, unknown pipeline, branch with
// no runs, or a project that has never run.
func (s *Store) GetBadgeLatestRun(ctx context.Context, slug, tokenHash, pipelineName, branch string) (BadgeRun, bool, error) {
	if branch == "" {
		defaultBranch, err := s.q.GetBadgeDefaultBranch(ctx, db.GetBadgeDefaultBranchParams{
			Slug:           slug,
			BadgeTokenHash: &tokenHash,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return BadgeRun{}, false, nil
		}
		if err != nil {
			return BadgeRun{}, false, fmt.Errorf("store: get badge default branch: %w", err)
		}
		branch = defaultBranch
	}
	if branch == "" {
		row, err := s.q.GetBadgeLatestRun(ctx, db.GetBadgeLatestRunParams{
			Slug:           slug,
			BadgeTokenHash: &tokenHash,
			PipelineName:   pipelineName,
		})
		return badgeRunFromRow(row.ID, row.Status, row.CreatedAt, row.PipelineName, err)
	}
	row, err := s.q.GetBadgeLatestRunByBranch(ctx, db.GetBadgeLatestRunByBranchParams{
		Branch:         branch,
		Slug:           slug,
		BadgeTokenHash: &tokenHash,
		PipelineName:   pipelineName,
	})
	return badgeRunFromRow(row.ID, row.Status, row.CreatedAt, row.PipelineName, err)
}

func badgeRunFromRow(id pgtype.UUID, status string, created pgtype.Timestamptz, pipelineName string, err error) (BadgeRun, bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return BadgeRun{}, false, nil
	}
	if err != nil {
		return BadgeRun{}, false, fmt.Errorf("store: get badge latest run: %w", err)
	}
	createdAt := time.Time{}
	if created.Valid {
		createdAt = created.Time
	}
	return BadgeRun{
		ID:           fromPgUUID(id),
		Status:       status,
		PipelineName: pipelineName,
		CreatedAt:    createdAt,
	}, true, nil
}
