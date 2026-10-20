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

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// CacheScopeShared is the lane every cache key had before PR-safe
// scoping: the project-wide blob. Only a push / integration to the
// project default branch writes here; every other lane reads it as a
// fallback base but writes to its own lane. Kept as "@" — a byte git
// refs can't contain, so it never collides with a derived lane name.
const CacheScopeShared = "@"

// sharedLaneCauses is the ALLOWLIST of triggers permitted to write the
// shared "@" lane (when they land on the default branch). Anything not
// here — pull_request, merge_group, tag, AND any future/unknown cause —
// is isolated, so a new trigger type can never silently clobber the
// shared cache. This is the fail-safe direction: adding a cause that
// SHOULD share the lane is a deliberate one-line edit; forgetting one
// just costs that trigger a cold first build, never corruption.
var sharedLaneCauses = map[string]bool{
	string(domain.CauseWebhook):  true, // branch push (default label)
	string(domain.CauseManual):   true, // manual run / deploy of the branch
	string(domain.CauseSchedule): true, // cron on the branch
	string(domain.CausePoll):     true, // SCM poll of the branch
	string(domain.CauseUpstream): true, // fanout carrying the parent's lane
}

// CacheScopeFor decides which cache lane a run writes to, from the
// trigger (cause), the run's denormalised lane ref (runs.ref — the
// branch, or "pr:<N>" for a pull_request, or the ephemeral
// "gh-readonly-queue/..." head ref for a merge_group, or "" for tag /
// manual-no-branch) and the project default branch.
//
// Only a cause on the allowlist, landing on the default branch, owns the
// shared "@" lane. Everything else — feature-branch push, PR, merge_group,
// tag, and any unknown cause — gets an isolated "<cause>/<ref>" lane so
// its writes can never clobber the shared blob or another ref's. The
// allowlist (not a denylist) is what actually guards "@": a trigger must
// be explicitly listed to write it, so a future cause defaults to
// isolation. The cause prefix also keeps lanes from colliding across
// trigger types (a branch literally named "pr:5" pushed can't land in
// PR #5's lane).
func CacheScopeFor(cause, ref, defaultBranch string) string {
	if sharedLaneCauses[cause] && ref != "" && ref == defaultBranch {
		return CacheScopeShared
	}
	return scopeLane(cause, ref)
}

// scopeLane builds a non-shared lane name. cause defaults to "run" so a
// legacy row with an empty cause still produces a stable, non-"@" lane.
//
// NOTE on ref fidelity: ref is runs.ref, capped at 255 bytes at write
// time (store.insertRunSkeleton). Two refs sharing a 255-byte prefix
// would therefore collapse into one lane and overwrite each other —
// which IS the clobbering class this feature exists to prevent, just
// pushed to a pathological corner (real git refs are far shorter than
// 255 bytes, and it can't cross project boundaries). Follow-up: key the
// lane off the full ref or a hash of it. Tracked in the PR discussion.
func scopeLane(cause, ref string) string {
	if cause == "" {
		cause = "run"
	}
	return cause + "/" + ref
}

// ResolveCacheScope loads the run's cause + lane ref and the project
// default branch, then maps them to a cache lane. A missing run or SCM
// source degrades to the shared lane's inputs rather than erroring: the
// cache path must never fail a job, and the worst case (no SCM → no
// default branch known) just means no run writes "@", which is the safe
// direction (isolation over sharing).
func (s *Store) ResolveCacheScope(ctx context.Context, runID, projectID uuid.UUID) (string, error) {
	var cause, ref string
	err := s.pool.QueryRow(ctx,
		`SELECT cause, ref FROM runs WHERE id = $1`, runID).Scan(&cause, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		// No run row (shouldn't happen past auth) — isolate by runID so a
		// stray caller can't write the shared lane.
		return scopeLane("run", runID.String()), nil
	}
	if err != nil {
		return "", fmt.Errorf("store: resolve cache scope: run lookup: %w", err)
	}

	var defaultBranch string
	err = s.pool.QueryRow(ctx,
		`SELECT default_branch FROM scm_sources WHERE project_id = $1`, projectID).
		Scan(&defaultBranch)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store: resolve cache scope: default branch: %w", err)
	}
	// pgx.ErrNoRows → defaultBranch stays "" → CacheScopeFor never returns
	// "@", so a project without an SCM source isolates every lane.

	return CacheScopeFor(cause, ref, defaultBranch), nil
}

// cacheStorageKeyScoped is the blob path for a (project, key, scope).
// The shared lane hashes identically to the legacy CacheStorageKey
// (sha256 of just the key) so pre-scope rows keep their stored blob;
// other lanes fold the scope into the hash so each ref gets a distinct
// object. The NUL separator keeps scope+key unambiguous.
func cacheStorageKeyScoped(projectID uuid.UUID, key, scope string) string {
	if scope == "" || scope == CacheScopeShared {
		return CacheStorageKey(projectID, key)
	}
	sum := sha256.Sum256([]byte(scope + "\x00" + key))
	return fmt.Sprintf("cache/%s/%s", projectID, hex.EncodeToString(sum[:]))
}

// UpsertPendingCacheScoped marks a fresh upload in flight for
// (project_id, key, scope). First call on a new lane inserts; a repeat
// flips the row back to pending so GETs miss until MarkCacheReady. The
// storage_key is stable per lane, so re-uploads overwrite in place.
func (s *Store) UpsertPendingCacheScoped(ctx context.Context, projectID uuid.UUID, key, scope string) (Cache, error) {
	if key == "" {
		return Cache{}, fmt.Errorf("store: cache key required")
	}
	if scope == "" {
		scope = CacheScopeShared
	}
	storageKey := cacheStorageKeyScoped(projectID, key, scope)
	var c Cache
	err := s.pool.QueryRow(ctx, `
		INSERT INTO caches (project_id, key, scope, storage_key, status)
		VALUES ($1, $2, $3, $4, 'pending')
		ON CONFLICT (project_id, key, scope) DO UPDATE
		SET status     = 'pending',
		    updated_at = NOW()
		RETURNING id, project_id, key, scope, storage_key, size_bytes,
		          COALESCE(content_sha256, ''), status,
		          created_at, updated_at, last_accessed_at
	`, projectID, key, scope, storageKey).Scan(
		&c.ID, &c.ProjectID, &c.Key, &c.Scope, &c.StorageKey, &c.SizeBytes,
		&c.ContentSHA256, &c.Status, &c.CreatedAt, &c.UpdatedAt, &c.LastAccessedAt,
	)
	if err != nil {
		return Cache{}, fmt.Errorf("store: upsert pending cache: %w", err)
	}
	return c, nil
}

// GetReadyCacheByKeyScoped returns the ready blob for (project, key)
// in the given lane, falling back to the shared "@" lane on a miss —
// the GitHub-Actions read model: a ref reads its own cache, then the
// default-branch cache as a base. Returns ErrCacheNotFound only when
// neither lane has a ready row. Bumps last_accessed_at on whichever
// lane answered so a lane actively read (including "@" read by many
// PRs) stays warm against the TTL sweep.
func (s *Store) GetReadyCacheByKeyScoped(ctx context.Context, projectID uuid.UUID, key, scope string) (Cache, error) {
	if scope == "" {
		scope = CacheScopeShared
	}
	c, err := s.getReadyCacheExact(ctx, projectID, key, scope)
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, ErrCacheNotFound) {
		return Cache{}, err
	}
	if scope == CacheScopeShared {
		return Cache{}, ErrCacheNotFound
	}
	return s.getReadyCacheExact(ctx, projectID, key, CacheScopeShared)
}

// getReadyCacheExact is the single-lane lookup behind the scoped read:
// no fallback, bumps last_accessed_at on the matched lane.
func (s *Store) getReadyCacheExact(ctx context.Context, projectID uuid.UUID, key, scope string) (Cache, error) {
	var c Cache
	err := s.pool.QueryRow(ctx, `
		UPDATE caches
		SET last_accessed_at = NOW()
		WHERE project_id = $1 AND key = $2 AND scope = $3 AND status = 'ready'
		RETURNING id, project_id, key, scope, storage_key, size_bytes,
		          COALESCE(content_sha256, ''), status,
		          created_at, updated_at, last_accessed_at
	`, projectID, key, scope).Scan(
		&c.ID, &c.ProjectID, &c.Key, &c.Scope, &c.StorageKey, &c.SizeBytes,
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

// ListExpiredCachesScoped returns ready rows past their lane's TTL:
// sharedTTL governs the "@" lane, scopedTTL the isolated ref lanes
// (shorter by default — a merged/closed PR's cache should surrender
// disk fast). A non-positive scopedTTL falls back to sharedTTL so
// callers that don't care about lanes behave like the flat sweep.
func (s *Store) ListExpiredCachesScoped(ctx context.Context, sharedTTL, scopedTTL time.Duration, limit int) ([]ExpiredCache, error) {
	if sharedTTL <= 0 {
		return nil, fmt.Errorf("store: cache ttl must be positive")
	}
	if scopedTTL <= 0 {
		scopedTTL = sharedTTL
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, storage_key, size_bytes
		FROM caches
		WHERE status = 'ready'
		  AND last_accessed_at < NOW() - make_interval(secs =>
		        CASE WHEN scope = $1 THEN $2::double precision
		             ELSE $3::double precision END)
		ORDER BY last_accessed_at ASC
		LIMIT $4
	`, CacheScopeShared, sharedTTL.Seconds(), scopedTTL.Seconds(), limit)
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

// ListExpiredPendingCaches returns rows stuck in `pending` past a grace
// window measured from updated_at — an agent that requested a PUT ticket
// and then died (before or after the blob upload, but before
// MarkCacheReady) leaves one of these. Pre-scoping the leak was bounded
// to one row per (project, key) because the upsert reused it; with
// per-ref lanes it grows with branch/PR/tag churn, so the sweeper must
// reclaim them. The grace must comfortably exceed the signed-PUT-URL TTL
// (the longest a legitimate upload can still be in flight) so a slow-but-
// live upload is never reaped mid-flight. Blob delete is idempotent, so
// it's safe whether or not the agent got as far as uploading.
func (s *Store) ListExpiredPendingCaches(ctx context.Context, grace time.Duration, limit int) ([]ExpiredCache, error) {
	if grace <= 0 {
		return nil, fmt.Errorf("store: cache pending grace must be positive")
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, storage_key, size_bytes
		FROM caches
		WHERE status = 'pending'
		  AND updated_at < NOW() - make_interval(secs => $1)
		ORDER BY updated_at ASC
		LIMIT $2
	`, grace.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list expired pending caches: %w", err)
	}
	defer rows.Close()

	var out []ExpiredCache
	for rows.Next() {
		var ec ExpiredCache
		if err := rows.Scan(&ec.ID, &ec.StorageKey, &ec.SizeBytes); err != nil {
			return nil, fmt.Errorf("store: scan expired pending cache: %w", err)
		}
		out = append(out, ec)
	}
	return out, rows.Err()
}

// ClaimStalePendingCache atomically deletes one pending row ONLY if it is
// still pending AND still past the grace, returning its storage key so
// the caller can then delete the blob. claimed=false means a concurrent
// RequestCachePut renewed it (bumped updated_at) or MarkCacheReady
// flipped it to ready in the window between the sweeper listing the row
// and acting on it — in which case we must NOT touch its blob, because a
// live upload session now owns it. This is the race fix for the
// list-then-delete gap: the row delete is the claim, and the blob delete
// only follows a successful claim (never the reverse), so a renewed
// upload keeps both its row and its object.
func (s *Store) ClaimStalePendingCache(ctx context.Context, id uuid.UUID, grace time.Duration) (ExpiredCache, bool, error) {
	if grace <= 0 {
		return ExpiredCache{}, false, fmt.Errorf("store: cache pending grace must be positive")
	}
	var ec ExpiredCache
	err := s.pool.QueryRow(ctx, `
		DELETE FROM caches
		WHERE id = $1
		  AND status = 'pending'
		  AND updated_at < NOW() - make_interval(secs => $2)
		RETURNING id, storage_key, size_bytes
	`, id, grace.Seconds()).Scan(&ec.ID, &ec.StorageKey, &ec.SizeBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExpiredCache{}, false, nil
	}
	if err != nil {
		return ExpiredCache{}, false, fmt.Errorf("store: claim stale pending cache: %w", err)
	}
	return ec, true, nil
}
