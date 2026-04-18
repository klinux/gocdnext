package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// ErrSCMSourceNotFound is returned by FindSCMSourceByURL / GetProjectByID
// when no row matches.
var (
	ErrSCMSourceNotFound = errors.New("store: scm_source not found")
	ErrProjectByIDNotFound = errors.New("store: project not found")
)

// SCMSource mirrors the scm_sources row for read paths (webhook drift match,
// future UI listings).
type SCMSource struct {
	ID                   uuid.UUID
	ProjectID            uuid.UUID
	Provider             string
	URL                  string
	DefaultBranch        string
	WebhookSecret        string
	AuthRef              string
	LastSyncedAt         *time.Time
	LastSyncedRevision   string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// ProjectInfo is the thin shape used by drift re-apply to rebuild an
// ApplyProjectInput — we only need slug/name/description.
type ProjectInfo struct {
	ID          uuid.UUID
	Slug        string
	Name        string
	Description string
}

// FindSCMSourceByURL looks up the scm_source bound to a given repo URL. The
// url is canonicalized with domain.NormalizeGitURL so webhook clone_url
// ("https://github.com/org/repo.git") matches a stored url without the
// ".git" suffix.
func (s *Store) FindSCMSourceByURL(ctx context.Context, rawURL string) (SCMSource, error) {
	row, err := s.q.FindScmSourceByURL(ctx, domain.NormalizeGitURL(rawURL))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SCMSource{}, ErrSCMSourceNotFound
		}
		return SCMSource{}, fmt.Errorf("store: find scm_source: %w", err)
	}
	return SCMSource{
		ID:                 fromPgUUID(row.ID),
		ProjectID:          fromPgUUID(row.ProjectID),
		Provider:           row.Provider,
		URL:                row.Url,
		DefaultBranch:      row.DefaultBranch,
		WebhookSecret:      stringValue(row.WebhookSecret),
		AuthRef:            stringValue(row.AuthRef),
		LastSyncedAt:       pgTimePtr(row.LastSyncedAt),
		LastSyncedRevision: stringValue(row.LastSyncedRevision),
		CreatedAt:          row.CreatedAt.Time,
		UpdatedAt:          row.UpdatedAt.Time,
	}, nil
}

// MarkSCMSourceSynced records a successful drift re-apply. Idempotent at the
// SQL level — safe to call with the same revision twice.
func (s *Store) MarkSCMSourceSynced(ctx context.Context, id uuid.UUID, revision string) error {
	if err := s.q.UpdateScmSourceSynced(ctx, db.UpdateScmSourceSyncedParams{
		ID: pgUUID(id), LastSyncedRevision: nullableString(revision),
	}); err != nil {
		return fmt.Errorf("store: mark scm_source synced: %w", err)
	}
	return nil
}

// GetProjectByID fetches the minimal project shape by UUID. Used by the
// drift path to rebuild the ApplyProjectInput when the caller only has a
// project_id from an scm_source row.
func (s *Store) GetProjectByID(ctx context.Context, id uuid.UUID) (ProjectInfo, error) {
	row, err := s.q.GetProjectByID(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProjectInfo{}, ErrProjectByIDNotFound
		}
		return ProjectInfo{}, fmt.Errorf("store: get project by id: %w", err)
	}
	return ProjectInfo{
		ID:          fromPgUUID(row.ID),
		Slug:        row.Slug,
		Name:        row.Name,
		Description: stringValue(row.Description),
	}, nil
}
