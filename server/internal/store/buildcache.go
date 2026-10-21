package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Embedded build cache (Gradle HttpBuildCache protocol) token management.
// The cache endpoint authenticates a build with a per-project token; only its
// SHA-256 hash is persisted (projects.cache_token_hash, migration 00094). A
// NULL hash means the embedded build cache is disabled for the project. Raw
// pool queries (not sqlc) keep this self-contained, matching caches.go.

// SetProjectCacheTokenHashBySlug enables or rotates the build-cache token. The
// caller owns token generation + hashing; the store never sees the plaintext.
func (s *Store) SetProjectCacheTokenHashBySlug(ctx context.Context, slug, tokenHash string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE projects SET cache_token_hash = $2, updated_at = NOW() WHERE slug = $1`,
		slug, tokenHash)
	if err != nil {
		return fmt.Errorf("store: set project cache token hash: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// ClearProjectCacheTokenBySlug disables the embedded build cache for a project.
func (s *Store) ClearProjectCacheTokenBySlug(ctx context.Context, slug string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE projects SET cache_token_hash = NULL, updated_at = NOW() WHERE slug = $1`,
		slug)
	if err != nil {
		return fmt.Errorf("store: clear project cache token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// GetProjectCacheEnabledBySlug reports whether the embedded build cache is
// enabled (a token hash is set) for a project, without exposing the hash.
func (s *Store) GetProjectCacheEnabledBySlug(ctx context.Context, slug string) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx,
		`SELECT cache_token_hash IS NOT NULL FROM projects WHERE slug = $1`, slug).
		Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrProjectNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: get project cache enabled: %w", err)
	}
	return enabled, nil
}

// ProjectCacheAuthBySlug resolves a slug to its project id and current cache
// credential generation — the stored cache_token_hash, or "" when the cache is
// disabled (NULL) or the slug is unknown. The build cache endpoint uses it to
// authorize minted tokens: the slug must map to the token's project, and the
// returned hash is the generation the token's signature must match. Because the
// hash changes on rotate and clears on disable, both revoke outstanding minted
// tokens immediately — including a disable→re-enable, which mints a new hash.
func (s *Store) ProjectCacheAuthBySlug(ctx context.Context, slug string) (uuid.UUID, string, error) {
	var id uuid.UUID
	var tokenHash string
	err := s.pool.QueryRow(ctx,
		`SELECT id, COALESCE(cache_token_hash, '') FROM projects WHERE slug = $1`, slug).
		Scan(&id, &tokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", nil
	}
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("store: project cache auth by slug: %w", err)
	}
	return id, tokenHash, nil
}

// VerifyProjectCacheToken resolves (slug, tokenHash) to the owning project id.
// ok=false covers every indistinguishable auth-failure case: unknown project,
// disabled cache (NULL hash never matches a hex digest), or wrong token. The
// match is a single indexed equality on the hash, so no plaintext comparison
// happens here and a NULL hash can never be satisfied by a caller-supplied one.
func (s *Store) VerifyProjectCacheToken(ctx context.Context, slug, tokenHash string) (uuid.UUID, bool, error) {
	if slug == "" || tokenHash == "" {
		return uuid.Nil, false, nil
	}
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM projects WHERE slug = $1 AND cache_token_hash = $2`,
		slug, tokenHash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("store: verify project cache token: %w", err)
	}
	return id, true, nil
}
