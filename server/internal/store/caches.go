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
	ID             uuid.UUID
	ProjectID      uuid.UUID
	Key            string
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

// UpsertPendingCache marks a fresh upload in flight for
// (project_id, key). First call on a new key inserts a row;
// subsequent calls on an existing key flip it back to `pending`
// so GETs treat the cache as "not ready" until MarkCacheReady
// fires. The StorageKey stays the same across uploads (the
// blob backend just overwrites), so we don't leak old objects
// and we don't need a delete path for replacement.
func (s *Store) UpsertPendingCache(ctx context.Context, projectID uuid.UUID, key string) (Cache, error) {
	if key == "" {
		return Cache{}, fmt.Errorf("store: cache key required")
	}
	storageKey := CacheStorageKey(projectID, key)
	var c Cache
	err := s.pool.QueryRow(ctx, `
		INSERT INTO caches (project_id, key, storage_key, status)
		VALUES ($1, $2, $3, 'pending')
		ON CONFLICT (project_id, key) DO UPDATE
		SET status     = 'pending',
		    updated_at = NOW()
		RETURNING id, project_id, key, storage_key, size_bytes,
		          COALESCE(content_sha256, ''), status,
		          created_at, updated_at, last_accessed_at
	`, projectID, key, storageKey).Scan(
		&c.ID, &c.ProjectID, &c.Key, &c.StorageKey, &c.SizeBytes,
		&c.ContentSHA256, &c.Status, &c.CreatedAt, &c.UpdatedAt, &c.LastAccessedAt,
	)
	if err != nil {
		return Cache{}, fmt.Errorf("store: upsert pending cache: %w", err)
	}
	return c, nil
}

// MarkCacheReady finalises an upload. Called by the agent after
// it confirms the blob backend accepted the PUT. The row flips
// to `ready`, picks up the size + sha256 the agent calculated,
// and becomes visible to GetReadyCacheByKey.
func (s *Store) MarkCacheReady(ctx context.Context, cacheID uuid.UUID, size int64, sha string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE caches
		SET status         = 'ready',
		    size_bytes     = $2,
		    content_sha256 = $3,
		    updated_at     = NOW()
		WHERE id = $1
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

// ListExpiredCaches returns ready rows whose last_accessed_at fell
// past the TTL window. Bounded by `limit` so a sweep tick doesn't
// try to reclaim 100k rows in one pass. Caller must delete the
// blob first, then DeleteCacheRow — order matters for the failure
// modes (blob lingers > row lingers, both are self-healing via
// next tick).
func (s *Store) ListExpiredCaches(ctx context.Context, ttl time.Duration, limit int) ([]ExpiredCache, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("store: cache ttl must be positive")
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, storage_key, size_bytes
		FROM caches
		WHERE status = 'ready'
		  AND last_accessed_at < NOW() - make_interval(secs => $1)
		ORDER BY last_accessed_at ASC
		LIMIT $2
	`, ttl.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list expired caches: %w", err)
	}
	defer rows.Close()

	var out []ExpiredCache
	for rows.Next() {
		var ec ExpiredCache
		if err := rows.Scan(&ec.ID, &ec.StorageKey, &ec.SizeBytes); err != nil {
			return nil, fmt.Errorf("store: scan expired cache: %w", err)
		}
		out = append(out, ec)
	}
	return out, rows.Err()
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

// DeleteCacheRow removes one row by id. Idempotent: a missing row
// returns nil (another sweeper beat us to it, which is fine —
// nothing to reclaim).
func (s *Store) DeleteCacheRow(ctx context.Context, id uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM caches WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete cache row: %w", err)
	}
	return nil
}

// GetReadyCacheByKey returns the ready blob for (project_id, key)
// or ErrCacheNotFound when there's no row, or one exists but is
// still pending. Bumps last_accessed_at so the eviction sweeper
// sees cache freshness — "used in the last 30 days" is the
// default LRU horizon (see roadmap_cache_eviction).
func (s *Store) GetReadyCacheByKey(ctx context.Context, projectID uuid.UUID, key string) (Cache, error) {
	var c Cache
	err := s.pool.QueryRow(ctx, `
		UPDATE caches
		SET last_accessed_at = NOW()
		WHERE project_id = $1 AND key = $2 AND status = 'ready'
		RETURNING id, project_id, key, storage_key, size_bytes,
		          COALESCE(content_sha256, ''), status,
		          created_at, updated_at, last_accessed_at
	`, projectID, key).Scan(
		&c.ID, &c.ProjectID, &c.Key, &c.StorageKey, &c.SizeBytes,
		&c.ContentSHA256, &c.Status, &c.CreatedAt, &c.UpdatedAt, &c.LastAccessedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cache{}, ErrCacheNotFound
	}
	if err != nil {
		return Cache{}, fmt.Errorf("store: get cache: %w", err)
	}
	return c, nil
}
