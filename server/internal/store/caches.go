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
