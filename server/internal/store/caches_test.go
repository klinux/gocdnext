package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedProject inserts a bare project so the caches FK lands.
// Uses ApplyProject with an empty pipeline list — the same
// no-op shape `New Project` + `Empty` uses.
func seedProject(t *testing.T, s *store.Store, slug string) uuid.UUID {
	t.Helper()
	res, err := s.ApplyProject(context.Background(), store.ApplyProjectInput{
		Slug:      slug,
		Name:      slug,
		Pipelines: []*domain.Pipeline{},
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return res.ProjectID
}

func TestCaches_UpsertThenMarkReadyRoundTrip(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "c1")

	// First upload: row goes pending, storage_key is deterministic.
	pending, err := s.UpsertPendingCache(ctx, projectID, "pnpm-store")
	if err != nil {
		t.Fatalf("upsert pending: %v", err)
	}
	if pending.Status != "pending" {
		t.Errorf("status = %q, want pending", pending.Status)
	}
	wantKey := store.CacheStorageKey(projectID, "pnpm-store")
	if pending.StorageKey != wantKey {
		t.Errorf("storage_key = %q, want %q", pending.StorageKey, wantKey)
	}

	// Pending row should NOT show up in reads yet.
	if _, err := s.GetReadyCacheByKey(ctx, projectID, "pnpm-store"); !errors.Is(err, store.ErrCacheNotFound) {
		t.Fatalf("pending cache leaked to read: err=%v", err)
	}

	// Agent reports success → flip to ready with metadata.
	if err := s.MarkCacheReady(ctx, pending.ID, 4096, "abc123"); err != nil {
		t.Fatalf("mark ready: %v", err)
	}

	ready, err := s.GetReadyCacheByKey(ctx, projectID, "pnpm-store")
	if err != nil {
		t.Fatalf("get ready: %v", err)
	}
	if ready.Status != "ready" || ready.SizeBytes != 4096 || ready.ContentSHA256 != "abc123" {
		t.Errorf("ready row = %+v", ready)
	}
}

func TestCaches_ReuploadOverwritesSameStorageKey(t *testing.T) {
	// Re-upload of the same (project, key) should reuse the
	// blob path (so the storage backend's overwrite semantics
	// handle the bytes) AND reset status to pending so a concurrent
	// GET blocks until MarkCacheReady finishes.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "c2")

	first, _ := s.UpsertPendingCache(ctx, projectID, "pnpm-store")
	if err := s.MarkCacheReady(ctx, first.ID, 1000, "sha1"); err != nil {
		t.Fatalf("first ready: %v", err)
	}

	// Second upload on the same key: same row id, same storage_key,
	// status back to pending.
	second, err := s.UpsertPendingCache(ctx, projectID, "pnpm-store")
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("row id changed across upserts: %s → %s", first.ID, second.ID)
	}
	if second.StorageKey != first.StorageKey {
		t.Errorf("storage_key changed: %q → %q", first.StorageKey, second.StorageKey)
	}
	if second.Status != "pending" {
		t.Errorf("status after reupload = %q, want pending", second.Status)
	}

	// A concurrent GET while the second upload is in flight has
	// to miss — otherwise it'd hand out torn data from the half-
	// written blob.
	if _, err := s.GetReadyCacheByKey(ctx, projectID, "pnpm-store"); !errors.Is(err, store.ErrCacheNotFound) {
		t.Fatalf("pending-after-reupload leaked: err=%v", err)
	}
}

func TestCaches_GetBumpsLastAccessedAt(t *testing.T) {
	// Eviction sweeper keys off last_accessed_at. Every GET must
	// bump it so an active cache doesn't age out.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "c3")

	c, _ := s.UpsertPendingCache(ctx, projectID, "k")
	_ = s.MarkCacheReady(ctx, c.ID, 1, "x")

	// Backdate last_accessed_at so the test can notice the bump.
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '1 hour' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	before, err := s.GetReadyCacheByKey(ctx, projectID, "k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// last_accessed_at was backdated an hour, GetReadyCacheByKey
	// is supposed to refresh it → within a few seconds of now.
	if before.LastAccessedAt.Before(before.UpdatedAt) {
		t.Errorf("last_accessed_at (%v) earlier than updated_at (%v) — didn't bump",
			before.LastAccessedAt, before.UpdatedAt)
	}
}

func TestCaches_MarkReady_UnknownIDErrors(t *testing.T) {
	// Guard: the agent code paths expect a clean ErrCacheNotFound
	// when the id it saved locally is gone (e.g. eviction ran
	// between UpsertPending and MarkReady).
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	err := s.MarkCacheReady(context.Background(), uuid.New(), 10, "x")
	if !errors.Is(err, store.ErrCacheNotFound) {
		t.Fatalf("unknown id err = %v, want ErrCacheNotFound", err)
	}
}

func TestCaches_ListExpiredCaches(t *testing.T) {
	// Sweeper contract: only `ready` rows past the TTL are
	// returned; pending rows and fresh-access rows stay put.
	// Ordering is oldest-first so a bounded batch always
	// reclaims the most stale rows.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "evict")

	// Three rows: stale-ready, fresh-ready, stale-pending.
	staleReady, _ := s.UpsertPendingCache(ctx, projectID, "stale-ready")
	_ = s.MarkCacheReady(ctx, staleReady.ID, 100, "a")
	freshReady, _ := s.UpsertPendingCache(ctx, projectID, "fresh-ready")
	_ = s.MarkCacheReady(ctx, freshReady.ID, 200, "b")
	stalePending, _ := s.UpsertPendingCache(ctx, projectID, "stale-pending")

	// Backdate the stale ones well past any reasonable TTL.
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '90 days' WHERE id = ANY($1)`,
		[]uuid.UUID{staleReady.ID, stalePending.ID}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	got, err := s.ListExpiredCaches(ctx, 30*24*time.Hour, 10)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 stale-ready row, got %d: %+v", len(got), got)
	}
	if got[0].ID != staleReady.ID {
		t.Errorf("wrong row returned: %s (want %s)", got[0].ID, staleReady.ID)
	}
	if got[0].SizeBytes != 100 {
		t.Errorf("size = %d, want 100", got[0].SizeBytes)
	}
}

func TestCaches_DeleteCacheRow(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "evict-delete")

	c, _ := s.UpsertPendingCache(ctx, projectID, "k")
	_ = s.MarkCacheReady(ctx, c.ID, 1, "x")

	if err := s.DeleteCacheRow(ctx, c.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetReadyCacheByKey(ctx, projectID, "k"); !errors.Is(err, store.ErrCacheNotFound) {
		t.Fatalf("row still present after delete: err=%v", err)
	}

	// Idempotent: deleting an already-gone id is not an error.
	// The sweeper relies on this — two instances racing would
	// otherwise spam the logs with "missing row" warnings.
	if err := s.DeleteCacheRow(ctx, c.ID); err != nil {
		t.Errorf("second delete should be no-op, got: %v", err)
	}
}

func TestCaches_GlobalUsageAndOldestList(t *testing.T) {
	// The global quota pass needs two inputs: total ready bytes
	// and an LRU-ordered candidate list that crosses project
	// boundaries. Pin both together so a future refactor can't
	// silently change the contract the sweeper relies on.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projA := seedProject(t, s, "proj-a")
	projB := seedProject(t, s, "proj-b")

	ca, _ := s.UpsertPendingCache(ctx, projA, "a")
	_ = s.MarkCacheReady(ctx, ca.ID, 100, "x")
	cb, _ := s.UpsertPendingCache(ctx, projB, "b")
	_ = s.MarkCacheReady(ctx, cb.ID, 200, "y")
	// Pending row must NOT count — still in flight.
	_, _ = s.UpsertPendingCache(ctx, projA, "pending")

	total, err := s.GlobalCacheUsage(ctx)
	if err != nil {
		t.Fatalf("global usage: %v", err)
	}
	if total != 300 {
		t.Errorf("GlobalCacheUsage = %d, want 300 (pending excluded)", total)
	}

	// ca is older → comes first in LRU.
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '1 hour' WHERE id = $1`, ca.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListOldestCachesGlobally(ctx, 10)
	if err != nil {
		t.Fatalf("list oldest: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (pending excluded): %+v", len(got), got)
	}
	if got[0].ID != ca.ID {
		t.Errorf("LRU head = %s, want %s (older project-A row)", got[0].ID, ca.ID)
	}
}

func TestCaches_ListCachesByProject_IncludesPendingAndReady(t *testing.T) {
	// Operator-facing list: both statuses show up so a stuck
	// upload can be spotted + cleaned manually. Ordering is
	// last_accessed_at DESC so the live keys lead the list.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "list-proj")

	ready, _ := s.UpsertPendingCache(ctx, projectID, "live-key")
	_ = s.MarkCacheReady(ctx, ready.ID, 999, "sha")
	pending, _ := s.UpsertPendingCache(ctx, projectID, "stuck-key")

	// Backdate the pending row so list ordering is deterministic.
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '1 day' WHERE id = $1`, pending.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListCachesByProject(ctx, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (both statuses): %+v", len(got), got)
	}
	if got[0].Status != "ready" || got[1].Status != "pending" {
		t.Errorf("ordering wrong: %s then %s", got[0].Status, got[1].Status)
	}
}

func TestCaches_GetCacheForProject_CrossProjectIsolation(t *testing.T) {
	// Row id from project A must not leak across project B's
	// GetCacheForProject call. Ensures purge handlers can trust
	// the ownership guard without re-checking in app code.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projA := seedProject(t, s, "own-a")
	projB := seedProject(t, s, "own-b")

	c, _ := s.UpsertPendingCache(ctx, projA, "k")
	_ = s.MarkCacheReady(ctx, c.ID, 1, "x")

	// Owner lookup succeeds.
	if _, err := s.GetCacheForProject(ctx, projA, c.ID); err != nil {
		t.Fatalf("owner lookup: %v", err)
	}
	// Foreign lookup surfaces ErrCacheNotFound (same treatment
	// as "id doesn't exist", no leaking existence).
	if _, err := s.GetCacheForProject(ctx, projB, c.ID); !errors.Is(err, store.ErrCacheNotFound) {
		t.Errorf("foreign lookup err = %v, want ErrCacheNotFound", err)
	}
}

func TestCaches_StorageKeyDeterministic(t *testing.T) {
	// CacheStorageKey is public and consumed by the eviction
	// sweeper to call storage.Delete. Pin the shape so a future
	// refactor doesn't silently change blob paths and leave
	// orphans in buckets.
	projID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	got := store.CacheStorageKey(projID, "pnpm-store")
	want := "cache/11111111-1111-1111-1111-111111111111/" +
		"caa68f9a2cc395f7f42931326f6aa994b9ec630716fa745daa9f4ebf6b6630a3"
	if got != want {
		t.Errorf("storage key = %q, want %q", got, want)
	}

	// Same key twice → identical path (that's the "same blob
	// overwrites" guarantee the reupload path relies on).
	if store.CacheStorageKey(projID, "pnpm-store") != got {
		t.Error("storage key not stable across calls")
	}
	// Different key → different path (so two caches in the
	// same project don't collide).
	if store.CacheStorageKey(projID, "go-build") == got {
		t.Error("different keys should produce different storage paths")
	}
}
