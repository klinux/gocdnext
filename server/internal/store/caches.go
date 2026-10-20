package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrCacheNotFound signals "no ready row for this (project, key)".
// Pending rows are NOT returned — the reader uses miss semantics
// (run without pre-populated cache) while an upload is still in
// flight so partial data never reaches the downstream job.
var ErrCacheNotFound = errors.New("store: cache entry not found")

// Cache is a materialised cache row — what the agent gets back
// on a successful lookup. StorageKey is deterministic per
// (project, key) pair so re-uploads overwrite the same blob
// without the DB needing to orphan-collect old storage objects.
type Cache struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	Key       string
	// Scope is the ref lane this blob belongs to: CacheScopeShared
	// ("@") for the project default branch, or "<cause>/<ref>" for an
	// isolated branch / PR / merge_group / tag lane. See cache_scope.go.
	Scope          string
	StorageKey     string
	SizeBytes      int64
	ContentSHA256  string
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastAccessedAt time.Time
}

// CacheStorageKey is the deterministic blob path the artifact
// backend receives. sha256 of the user-supplied key (which could
// contain `/` or other characters not safe on S3 / filesystem
// paths) gives a fixed-width hex we can safely interpolate.
// Exposed so the sweeper can issue Delete(key) by the same rule.
func CacheStorageKey(projectID uuid.UUID, key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("cache/%s/%s", projectID, hex.EncodeToString(sum[:]))
}

// UpsertPendingCache marks a fresh upload in flight on the shared
// ("@") lane for (project_id, key). Thin wrapper over
// UpsertPendingCacheScoped — the scope-aware cache RPCs call the scoped
// method with the run's lane; this one preserves the pre-scoping
// signature for callers and tests that only ever meant the shared lane.
func (s *Store) UpsertPendingCache(ctx context.Context, projectID uuid.UUID, key string) (Cache, error) {
	return s.UpsertPendingCacheScoped(ctx, projectID, key, CacheScopeShared)
}

// MarkCacheReady finalises an upload. Called by the agent after
// it confirms the blob backend accepted the PUT. The row flips
// to `ready`, picks up the size + sha256 the agent calculated,
// and becomes visible to GetReadyCacheByKey. last_accessed_at is
// bumped too: a row that sat `pending` for a while (first upload,
// or a re-upload over a long-idle row) is freshly usable now, so it
// must start its TTL clock from here — otherwise a stale timestamp
// could make the sweeper evict a just-completed cache.
//
// A row the sweeper has tombstoned (status='deleting', its blob being
// reclaimed) is NOT promoted — the `status <> 'deleting'` guard makes
// this a no-op → ErrCacheNotFound, so a finalise racing a reclaim can't
// resurrect a row whose blob is about to vanish.
func (s *Store) MarkCacheReady(ctx context.Context, cacheID uuid.UUID, size int64, sha string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE caches
		SET status           = 'ready',
		    size_bytes       = $2,
		    content_sha256   = $3,
		    updated_at       = NOW(),
		    last_accessed_at = NOW()
		WHERE id = $1 AND status <> 'deleting'
	`, cacheID, size, sha)
	if err != nil {
		return fmt.Errorf("store: mark cache ready: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCacheNotFound
	}
	return nil
}

// ExpiredCache is what the sweeper needs to evict one row: the id
// to DELETE and the storage key to hand to the blob backend. Size
// rides along so the sweep stats can report bytes reclaimed.
type ExpiredCache struct {
	ID         uuid.UUID
	StorageKey string
	SizeBytes  int64
}

// ListExpiredCaches returns ready rows whose last_accessed_at fell past
// a single TTL window applied to every lane. Thin wrapper over
// ListExpiredCachesScoped (shared and scoped TTL equal) — the sweeper
// calls the scoped method so non-"@" lanes can age out faster. Bounded
// by `limit`; caller deletes the blob first, then DeleteCacheRow (both
// self-healing via the next tick).
func (s *Store) ListExpiredCaches(ctx context.Context, ttl time.Duration, limit int) ([]ExpiredCache, error) {
	return s.ListExpiredCachesScoped(ctx, ttl, ttl, limit)
}

// ProjectCacheUsage pairs a project id with its current total
// `ready` cache bytes. Returned by ListProjectsOverCacheQuota so
// the sweeper can compute `excess = Bytes - quota` per project.
type ProjectCacheUsage struct {
	ProjectID uuid.UUID
	Bytes     int64
}

// ListProjectsOverCacheQuota returns every project whose total
// `ready` cache bytes exceeds `quotaBytes`. Called once per
// sweeper tick; projects under quota aren't returned so a 10k-
// project deployment doesn't iterate the world. Pending rows
// don't count — they're ephemeral and still in flight.
func (s *Store) ListProjectsOverCacheQuota(ctx context.Context, quotaBytes int64) ([]ProjectCacheUsage, error) {
	if quotaBytes <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT project_id, SUM(size_bytes)::bigint
		FROM caches
		WHERE status = 'ready'
		GROUP BY project_id
		HAVING SUM(size_bytes) > $1
	`, quotaBytes)
	if err != nil {
		return nil, fmt.Errorf("store: list over-quota projects: %w", err)
	}
	defer rows.Close()
	var out []ProjectCacheUsage
	for rows.Next() {
		var u ProjectCacheUsage
		if err := rows.Scan(&u.ProjectID, &u.Bytes); err != nil {
			return nil, fmt.Errorf("store: scan project usage: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListOldestCachesInProject returns the N oldest-accessed `ready`
// rows in a project, bounded by `limit`. Caller picks enough rows
// off this list to free `bytesToFree`, then deletes them (blob +
// row) with the same loop the TTL sweep uses. LRU by
// last_accessed_at so active builds keep their caches and
// abandoned keys go first.
func (s *Store) ListOldestCachesInProject(ctx context.Context, projectID uuid.UUID, limit int) ([]ExpiredCache, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, storage_key, size_bytes
		FROM caches
		WHERE status = 'ready' AND project_id = $1
		ORDER BY last_accessed_at ASC
		LIMIT $2
	`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list oldest caches: %w", err)
	}
	defer rows.Close()
	var out []ExpiredCache
	for rows.Next() {
		var ec ExpiredCache
		if err := rows.Scan(&ec.ID, &ec.StorageKey, &ec.SizeBytes); err != nil {
			return nil, fmt.Errorf("store: scan oldest cache: %w", err)
		}
		out = append(out, ec)
	}
	return out, rows.Err()
}

// GlobalCacheUsage returns the total byte count across every
// `ready` cache row. Used by the sweeper's global quota pass to
// decide whether any eviction is needed this tick. Pending rows
// are excluded — they're still in flight and their declared size
// is zero until MarkCacheReady fires.
func (s *Store) GlobalCacheUsage(ctx context.Context) (int64, error) {
	var total int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(size_bytes), 0)::bigint
		FROM caches
		WHERE status = 'ready'
	`).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: global cache usage: %w", err)
	}
	return total, nil
}

// ListOldestCachesGlobally returns the N oldest-accessed `ready`
// rows across the whole table, regardless of project. The global
// quota pass picks the minimum prefix of this list whose byte sum
// covers the overshoot. LRU ordering means abandoned projects
// lose their caches before an actively-building project does.
func (s *Store) ListOldestCachesGlobally(ctx context.Context, limit int) ([]ExpiredCache, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, storage_key, size_bytes
		FROM caches
		WHERE status = 'ready'
		ORDER BY last_accessed_at ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list oldest caches globally: %w", err)
	}
	defer rows.Close()
	var out []ExpiredCache
	for rows.Next() {
		var ec ExpiredCache
		if err := rows.Scan(&ec.ID, &ec.StorageKey, &ec.SizeBytes); err != nil {
			return nil, fmt.Errorf("store: scan global oldest cache: %w", err)
		}
		out = append(out, ec)
	}
	return out, rows.Err()
}

// ListCachesByProject returns every cache row owned by the
// project, ready or pending. Ordered by last_accessed_at DESC so
// the UI surfaces the most recently used keys at the top — what
// an operator usually wants when eyeballing "what's live?".
// Pending rows are included so an operator debugging a stuck
// upload sees them in the same list instead of hunting for a
// ghost.
func (s *Store) ListCachesByProject(ctx context.Context, projectID uuid.UUID) ([]Cache, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, key, scope, storage_key, size_bytes,
		       COALESCE(content_sha256, ''), status,
		       created_at, updated_at, last_accessed_at
		FROM caches
		WHERE project_id = $1
		ORDER BY last_accessed_at DESC, key ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list caches by project: %w", err)
	}
	defer rows.Close()
	var out []Cache
	for rows.Next() {
		var c Cache
		if err := rows.Scan(
			&c.ID, &c.ProjectID, &c.Key, &c.Scope, &c.StorageKey, &c.SizeBytes,
			&c.ContentSHA256, &c.Status, &c.CreatedAt, &c.UpdatedAt, &c.LastAccessedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan cache: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCacheForProject fetches a row by (project_id, id) so the
// ownership check happens in the same query that resolves the
// row — no TOCTOU window where a handler reads one thing and
// deletes another. Returns ErrCacheNotFound for both "id doesn't
// exist" and "id belongs to a different project" (treat as the
// same leak-preventing 404 at the HTTP layer).
func (s *Store) GetCacheForProject(ctx context.Context, projectID, cacheID uuid.UUID) (Cache, error) {
	var c Cache
	err := s.pool.QueryRow(ctx, `
		SELECT id, project_id, key, scope, storage_key, size_bytes,
		       COALESCE(content_sha256, ''), status,
		       created_at, updated_at, last_accessed_at
		FROM caches
		WHERE id = $1 AND project_id = $2
	`, cacheID, projectID).Scan(
		&c.ID, &c.ProjectID, &c.Key, &c.Scope, &c.StorageKey, &c.SizeBytes,
		&c.ContentSHA256, &c.Status, &c.CreatedAt, &c.UpdatedAt, &c.LastAccessedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cache{}, ErrCacheNotFound
	}
	if err != nil {
		return Cache{}, fmt.Errorf("store: get cache for project: %w", err)
	}
	return c, nil
}

// DeleteCacheRow removes one row by id. Idempotent: a missing row
// returns nil (another sweeper beat us to it, which is fine —
// nothing to reclaim).
func (s *Store) DeleteCacheRow(ctx context.Context, id uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM caches WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete cache row: %w", err)
	}
	return nil
}

// GetReadyCacheByKey reads the shared ("@") lane for (project_id, key).
// Thin wrapper over GetReadyCacheByKeyScoped — kept for callers and
// tests that predate PR-safe scoping; the scope-aware cache RPCs call
// the scoped method directly. Bumps last_accessed_at (LRU freshness for
// the eviction sweeper, see roadmap_cache_eviction).
func (s *Store) GetReadyCacheByKey(ctx context.Context, projectID uuid.UUID, key string) (Cache, error) {
	return s.GetReadyCacheByKeyScoped(ctx, projectID, key, CacheScopeShared)
}
