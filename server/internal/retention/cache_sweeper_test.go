package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedCacheProject mirrors seedProject from store tests — inserts
// a bare project through ApplyProject so the cache FK lands. The
// retention sweeper tests need several different projects so the
// sweep boundaries (stale vs fresh) are legible.
func seedCacheProject(t *testing.T, pool *pgxpool.Pool, slug string) uuid.UUID {
	t.Helper()
	res, err := store.New(pool).ApplyProject(context.Background(), store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{},
	})
	if err != nil {
		t.Fatalf("seed project %q: %v", slug, err)
	}
	return res.ProjectID
}

func TestSweeper_ExpiredCache_DeletesBlobAndRow(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()
	ctx := context.Background()
	projectID := seedCacheProject(t, pool, "cache-ttl")

	c, _ := s.UpsertPendingCache(ctx, projectID, "pnpm-store")
	_ = s.MarkCacheReady(ctx, c.ID, 4096, "abc")
	// Backdate so the row is past the default 30-day window.
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '60 days' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	sw := retention.New(s, fs, silent())
	stats := sw.SweepOnce(ctx)

	if stats.CachesDeleted != 1 {
		t.Errorf("CachesDeleted = %d, want 1 (stats=%+v)", stats.CachesDeleted, stats)
	}
	if stats.CacheBytesFreed != 4096 {
		t.Errorf("CacheBytesFreed = %d, want 4096", stats.CacheBytesFreed)
	}
	if fs.deleteCount(c.StorageKey) != 1 {
		t.Errorf("storage delete count = %d", fs.deleteCount(c.StorageKey))
	}
	if _, err := s.GetReadyCacheByKey(ctx, projectID, "pnpm-store"); err == nil {
		t.Error("cache row still present after sweep")
	}
}

func TestSweeper_FreshCache_IsKept(t *testing.T) {
	// Freshly-accessed cache must survive the sweep. Core invariant:
	// the sweeper only touches rows past the TTL; an active pipeline
	// must never lose its cache mid-run.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()
	ctx := context.Background()
	projectID := seedCacheProject(t, pool, "cache-fresh")

	c, _ := s.UpsertPendingCache(ctx, projectID, "k")
	_ = s.MarkCacheReady(ctx, c.ID, 1, "x")
	// last_accessed_at stays at NOW — well within the 30-day TTL.

	sw := retention.New(s, fs, silent())
	stats := sw.SweepOnce(ctx)

	if stats.CachesDeleted != 0 {
		t.Errorf("fresh row was deleted: %+v", stats)
	}
	if fs.deleteCount(c.StorageKey) != 0 {
		t.Errorf("storage delete count = %d, want 0", fs.deleteCount(c.StorageKey))
	}
}

func TestSweeper_PendingCache_IsKeptRegardlessOfAge(t *testing.T) {
	// A pending row might be backdated because an upload is in
	// flight on a long-running agent. Eviction must never touch
	// pending rows or the upload would land on a deleted
	// storage_key and the agent's MarkCacheReady would flip a
	// ghost row that's about to get reclaimed.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()
	ctx := context.Background()
	projectID := seedCacheProject(t, pool, "cache-pending")

	c, _ := s.UpsertPendingCache(ctx, projectID, "k")
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '60 days' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	sw := retention.New(s, fs, silent())
	stats := sw.SweepOnce(ctx)

	if stats.CachesDeleted != 0 {
		t.Errorf("pending row was swept: %+v", stats)
	}
}

func TestSweeper_CacheTTLDisabled_NoSweep(t *testing.T) {
	// Operator set GOCDNEXT_CACHE_TTL=0 → cache eviction off.
	// Even stale rows must be preserved so long-term deployments
	// can opt out entirely.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()
	ctx := context.Background()
	projectID := seedCacheProject(t, pool, "cache-disabled")

	c, _ := s.UpsertPendingCache(ctx, projectID, "k")
	_ = s.MarkCacheReady(ctx, c.ID, 10, "x")
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '1 year' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	sw := retention.New(s, fs, silent()).WithCacheTTL(0)
	stats := sw.SweepOnce(ctx)

	if stats.CachesDeleted != 0 {
		t.Errorf("cache was swept with TTL=0: %+v", stats)
	}
}

func TestSweeper_CacheStorageDeleteFailure_KeepsRow(t *testing.T) {
	// Transport hiccup on blob delete — row must stay so the next
	// tick retries. The contract is "self-healing via next tick"
	// rather than an explicit "deleting" status like artefacts
	// have, because caches are small and lossy retries aren't a
	// correctness problem.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()
	ctx := context.Background()
	projectID := seedCacheProject(t, pool, "cache-storage-fail")

	c, _ := s.UpsertPendingCache(ctx, projectID, "k")
	_ = s.MarkCacheReady(ctx, c.ID, 10, "x")
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '60 days' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	fs.failOn[c.StorageKey] = errSyntheticStorage{}

	sw := retention.New(s, fs, silent())
	stats := sw.SweepOnce(ctx)

	if stats.CacheStorageFailures != 1 {
		t.Errorf("CacheStorageFailures = %d, want 1 (stats=%+v)", stats.CacheStorageFailures, stats)
	}
	if stats.CachesDeleted != 0 {
		t.Errorf("row was deleted despite storage failure: %+v", stats)
	}
	// Row still queryable → next tick will retry.
	if _, err := s.GetReadyCacheByKey(ctx, projectID, "k"); err != nil {
		t.Errorf("row removed despite failure: %v", err)
	}
}

func TestSweeper_CacheProjectQuota_EvictsOldestUntilUnderLimit(t *testing.T) {
	// Quota = 150 bytes. Seed three 100-byte caches with
	// successively older last_accessed_at. The sweeper should
	// evict `c1` (oldest, 100 bytes) — that alone brings the
	// project from 300 → 200 → past quota-then-under: actually
	// 200 still > 150, so evict c2 too → total 100 < 150, stop.
	// Never touch c3 (newest) so the active build keeps warmth.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()
	ctx := context.Background()
	projectID := seedCacheProject(t, pool, "cache-quota")

	c1, _ := s.UpsertPendingCache(ctx, projectID, "oldest")
	_ = s.MarkCacheReady(ctx, c1.ID, 100, "x")
	c2, _ := s.UpsertPendingCache(ctx, projectID, "middle")
	_ = s.MarkCacheReady(ctx, c2.ID, 100, "x")
	c3, _ := s.UpsertPendingCache(ctx, projectID, "newest")
	_ = s.MarkCacheReady(ctx, c3.ID, 100, "x")

	// Stagger last_accessed_at so LRU ordering is deterministic.
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '3 hours' WHERE id = $1`, c1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '2 hours' WHERE id = $1`, c2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '1 hour' WHERE id = $1`, c3.ID); err != nil {
		t.Fatal(err)
	}

	sw := retention.New(s, fs, silent()).
		WithCacheTTL(0). // Isolate the quota path — no TTL interference.
		WithCacheProjectQuotaBytes(150)
	stats := sw.SweepOnce(ctx)

	if stats.CachesDeletedByQuota != 2 {
		t.Fatalf("CachesDeletedByQuota = %d, want 2 (stats=%+v)", stats.CachesDeletedByQuota, stats)
	}
	if stats.CachesDeleted != 0 {
		t.Errorf("CachesDeleted = %d (quota evictions should not count under the TTL counter)", stats.CachesDeleted)
	}
	if stats.CacheBytesFreed != 200 {
		t.Errorf("CacheBytesFreed = %d, want 200", stats.CacheBytesFreed)
	}
	// c3 (newest) must still be present — otherwise we over-evicted
	// and the next build pays for a miss that didn't help disk at all.
	if _, err := s.GetReadyCacheByKey(ctx, projectID, "newest"); err != nil {
		t.Errorf("newest cache was evicted: %v", err)
	}
	// c1 + c2 should be gone.
	if _, err := s.GetReadyCacheByKey(ctx, projectID, "oldest"); err == nil {
		t.Error("oldest cache still present after quota eviction")
	}
	if _, err := s.GetReadyCacheByKey(ctx, projectID, "middle"); err == nil {
		t.Error("middle cache still present after quota eviction")
	}
}

func TestSweeper_CacheProjectQuota_UnderLimitNoOp(t *testing.T) {
	// Usage below quota → nothing evicted. Pin the "don't
	// over-evict" invariant separately from the positive case.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()
	ctx := context.Background()
	projectID := seedCacheProject(t, pool, "cache-quota-under")

	c, _ := s.UpsertPendingCache(ctx, projectID, "k")
	_ = s.MarkCacheReady(ctx, c.ID, 100, "x")

	sw := retention.New(s, fs, silent()).
		WithCacheTTL(0).
		WithCacheProjectQuotaBytes(1024)
	stats := sw.SweepOnce(ctx)

	if stats.CachesDeletedByQuota != 0 {
		t.Errorf("under-quota project had evictions: %+v", stats)
	}
}

func TestSweeper_CacheProjectQuotaDisabled_NoSweep(t *testing.T) {
	// Operator didn't set a quota (default 0). Even an absurd
	// accumulation must be preserved — ops opted out, we oblige.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()
	ctx := context.Background()
	projectID := seedCacheProject(t, pool, "cache-quota-off")

	c, _ := s.UpsertPendingCache(ctx, projectID, "k")
	_ = s.MarkCacheReady(ctx, c.ID, 10_000_000_000, "x") // 10 GB

	sw := retention.New(s, fs, silent()).WithCacheTTL(0) // Quota default 0.
	stats := sw.SweepOnce(ctx)
	if stats.CachesDeletedByQuota != 0 {
		t.Errorf("quota=0 should disable: %+v", stats)
	}
}

func TestSweeper_CacheTTL_HonorsSnapshot(t *testing.T) {
	// Admin page reads Snapshot() — the cache knobs must round-
	// trip so ops can eyeball the effective window and cap.
	pool := dbtest.SetupPool(t)
	sw := retention.New(store.New(pool), newFakeStore(), silent()).
		WithCacheTTL(7 * 24 * time.Hour).
		WithCacheProjectQuotaBytes(2 * 1024 * 1024 * 1024)

	snap := sw.Snapshot()
	if snap.CacheTTL != 7*24*time.Hour {
		t.Errorf("CacheTTL = %v, want 7d", snap.CacheTTL)
	}
	if snap.CacheProjectQuotaBytes != 2*1024*1024*1024 {
		t.Errorf("CacheProjectQuotaBytes = %d, want 2 GiB", snap.CacheProjectQuotaBytes)
	}
}

type errSyntheticStorage struct{}

func (errSyntheticStorage) Error() string { return "synthetic: storage unavailable" }
