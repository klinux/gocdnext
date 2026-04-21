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

// SCMSource mirrors the scm_sources row for read paths (webhook
// drift match, future UI listings). Deliberately does NOT expose
// the webhook secret ciphertext — the only consumer is the webhook
// handler, which goes through FindSCMSourceWebhookSecret so the
// plaintext lives only inside that narrow call.
type SCMSource struct {
	ID                 uuid.UUID
	ProjectID          uuid.UUID
	Provider           string
	URL                string
	DefaultBranch      string
	AuthRef            string
	LastSyncedAt       *time.Time
	LastSyncedRevision string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ProjectInfo is the thin shape used by drift re-apply to
// rebuild an ApplyProjectInput + by the webhook fetch path to
// know which folder to read from the remote repo.
type ProjectInfo struct {
	ID          uuid.UUID
	Slug        string
	Name        string
	Description string
	ConfigPath  string
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
		AuthRef:            stringValue(row.AuthRef),
		LastSyncedAt:       pgTimePtr(row.LastSyncedAt),
		LastSyncedRevision: stringValue(row.LastSyncedRevision),
		CreatedAt:          row.CreatedAt.Time,
		UpdatedAt:          row.UpdatedAt.Time,
	}, nil
}

// FindSCMSourceByProjectSlug resolves the scm_source bound to a
// project by slug. Returns ErrSCMSourceNotFound when the project
// has no binding (or no such project). Used by the webhook-secret
// rotation endpoint so the caller doesn't have to go slug→project
// →scm_source in two round-trips.
func (s *Store) FindSCMSourceByProjectSlug(ctx context.Context, slug string) (SCMSource, error) {
	row, err := s.q.FindScmSourceBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SCMSource{}, ErrSCMSourceNotFound
		}
		return SCMSource{}, fmt.Errorf("store: find scm_source by slug: %w", err)
	}
	return SCMSource{
		ID:                 fromPgUUID(row.ID),
		ProjectID:          fromPgUUID(row.ProjectID),
		Provider:           row.Provider,
		URL:                row.Url,
		DefaultBranch:      row.DefaultBranch,
		AuthRef:            stringValue(row.AuthRef),
		LastSyncedAt:       pgTimePtr(row.LastSyncedAt),
		LastSyncedRevision: stringValue(row.LastSyncedRevision),
		CreatedAt:          row.CreatedAt.Time,
		UpdatedAt:          row.UpdatedAt.Time,
	}, nil
}

// SCMSourceWebhookAuth is the slim shape the webhook handler
// consumes: enough to identify which scm_source the request is
// for plus the plaintext secret needed for HMAC verification.
// Separate from SCMSource so the ciphertext never rides along
// the general read path.
type SCMSourceWebhookAuth struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	Secret    string
}

// FindSCMSourceWebhookSecret returns the decrypted webhook secret
// paired with the scm_source for a given repo URL. Errors:
//   - ErrSCMSourceNotFound when no row binds this URL
//   - ErrAuthProviderCipherUnset when the server was started
//     without GOCDNEXT_SECRET_KEY (can't decrypt)
// An empty Secret with nil error means "row exists but no secret
// registered yet" — the caller should answer 401 in that case.
func (s *Store) FindSCMSourceWebhookSecret(ctx context.Context, rawURL string) (SCMSourceWebhookAuth, error) {
	if s.authCipher == nil {
		return SCMSourceWebhookAuth{}, ErrAuthProviderCipherUnset
	}
	row, err := s.q.GetScmSourceWebhookSecretByURL(ctx, domain.NormalizeGitURL(rawURL))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SCMSourceWebhookAuth{}, ErrSCMSourceNotFound
		}
		return SCMSourceWebhookAuth{}, fmt.Errorf("store: get webhook secret: %w", err)
	}
	out := SCMSourceWebhookAuth{
		ID:        fromPgUUID(row.ID),
		ProjectID: fromPgUUID(row.ProjectID),
	}
	if len(row.WebhookSecret) > 0 {
		plain, err := s.authCipher.Decrypt(row.WebhookSecret)
		if err != nil {
			return SCMSourceWebhookAuth{}, fmt.Errorf("store: decrypt webhook secret: %w", err)
		}
		out.Secret = string(plain)
	}
	return out, nil
}

// RotateSCMSourceWebhookSecret generates a new random secret,
// encrypts it, replaces the stored ciphertext, and returns the
// plaintext to the caller. The plaintext lives only on the
// return value — subsequent reads can't recover it.
func (s *Store) RotateSCMSourceWebhookSecret(ctx context.Context, id uuid.UUID) (string, error) {
	if s.authCipher == nil {
		return "", ErrAuthProviderCipherUnset
	}
	plain, err := newWebhookSecret()
	if err != nil {
		return "", err
	}
	sealed, err := s.authCipher.Encrypt([]byte(plain))
	if err != nil {
		return "", fmt.Errorf("store: seal webhook secret: %w", err)
	}
	if err := s.q.UpdateScmSourceWebhookSecret(ctx, db.UpdateScmSourceWebhookSecretParams{
		ID:            pgUUID(id),
		WebhookSecret: sealed,
	}); err != nil {
		return "", fmt.Errorf("store: rotate webhook secret: %w", err)
	}
	return plain, nil
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
		ConfigPath:  row.ConfigPath,
	}, nil
}
