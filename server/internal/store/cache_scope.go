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
	string(domain.CauseSchedule): true, // reserved schedule cause
	string(domain.CausePoll):     true, // SCM poll of the branch
	string(domain.CauseUpstream): true, // fanout carrying the parent's lane
	// Per-pipeline cron stamps cause="cron" literally (cron.go), distinct
	// from CauseSchedule — a cron on the default branch is a legit shared-
	// lane build, so list it or it'd needlessly run cold on its own lane.
	"cron": true,
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

// ErrCacheReclaiming signals the lane's row is mid-reclaim (status
// 'deleting'): the sweeper has claimed it and is about to delete its blob,
// which shares the deterministic storage_key. Reviving it here would let
// the sweeper delete THIS upload's object. The cache RPC treats it as a
// soft PUT failure (the agent is fail-open: logs + the next run rebuilds).
var ErrCacheReclaiming = errors.New("store: cache lane is being reclaimed")

// UpsertPendingCacheScoped marks a fresh upload in flight for
// (project_id, key, scope). First call on a new lane inserts; a repeat
// flips the row back to pending so GETs miss until MarkCacheReady. The
// storage_key is stable per lane, so re-uploads overwrite in place.
//
// It will NOT revive a row the sweeper has tombstoned (status='deleting')
// — the WHERE guard makes the upsert a no-op in that case, returning
// ErrCacheReclaiming. Because the storage_key is deterministic, reviving a
// tombstone would point a fresh upload at a blob the sweeper is about to
// delete (see ClaimStalePendingCaches). The window is tiny (one sweep tick
// deletes the blob + row, then a plain INSERT succeeds) and non-corrupting.
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
		WHERE caches.status <> 'deleting'
		RETURNING id, project_id, key, scope, storage_key, size_bytes,
		          COALESCE(content_sha256, ''), status,
		          created_at, updated_at, last_accessed_at
	`, projectID, key, scope, storageKey).Scan(
		&c.ID, &c.ProjectID, &c.Key, &c.Scope, &c.StorageKey, &c.SizeBytes,
		&c.ContentSHA256, &c.Status, &c.CreatedAt, &c.UpdatedAt, &c.LastAccessedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// Conflict row is a tombstone; the DO UPDATE WHERE suppressed it.
		return Cache{}, ErrCacheReclaiming
	}
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

// ClaimStalePendingCaches atomically TOMBSTONES (status -> 'deleting') a
// bounded batch of reclaimable rows and returns their storage keys so the
// sweeper can delete each blob and then the row. Two populations are
// claimed, mirroring ClaimArtifactsForSweep:
//
//  1. `pending` rows past `grace` — an agent that got a PUT ticket and
//     died before MarkCacheReady. Pre-scoping this leak was bounded to one
//     row per (project, key); per-ref lanes make it grow with branch/PR/
//     tag churn. `grace` must exceed the signed-PUT-URL TTL so a slow-but-
//     live upload is never reaped mid-flight.
//  2. `deleting` rows past `retryGrace` — a previous sweep crashed between
//     the blob delete and the row delete. Re-claimed and retried
//     idempotently so a transient storage error can't orphan the object.
//
// Tombstoning (not deleting) is the claim: the storage_key is deterministic
// per lane, so if we deleted the row outright a concurrent RequestCachePut
// would recreate it with the SAME key and our pending blob delete would hit
// that fresh upload. The 'deleting' marker blocks revival
// (UpsertPendingCacheScoped) and promotion (MarkCacheReady), so the blob is
// ours to delete. FOR UPDATE SKIP LOCKED keeps it safe across sweepers.
func (s *Store) ClaimStalePendingCaches(ctx context.Context, grace, retryGrace time.Duration, limit int) ([]ExpiredCache, error) {
	if grace <= 0 {
		return nil, fmt.Errorf("store: cache pending grace must be positive")
	}
	if retryGrace <= 0 {
		retryGrace = grace
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE caches SET status = 'deleting', updated_at = NOW()
		WHERE id IN (
			SELECT id FROM caches
			WHERE (status = 'pending'  AND updated_at < NOW() - make_interval(secs => $1))
			   OR (status = 'deleting' AND updated_at < NOW() - make_interval(secs => $2))
			ORDER BY updated_at ASC
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, storage_key, size_bytes
	`, grace.Seconds(), retryGrace.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: claim stale pending caches: %w", err)
	}
	defer rows.Close()

	var out []ExpiredCache
	for rows.Next() {
		var ec ExpiredCache
		if err := rows.Scan(&ec.ID, &ec.StorageKey, &ec.SizeBytes); err != nil {
			return nil, fmt.Errorf("store: scan claimed pending cache: %w", err)
		}
		out = append(out, ec)
	}
	return out, rows.Err()
}
