package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// TestCacheScopeFor is the decision matrix for which cache lane a run
// writes to — the brains of PR-safe scoping, pure so it needs no DB.
// The invariant under test: ONLY a push/integration landing on the
// default branch owns the shared "@" lane; everything else is isolated,
// and merge_group (whose ref IS the default branch) must NOT leak into
// "@".
func TestCacheScopeFor(t *testing.T) {
	const def = "main"
	tests := []struct {
		name          string
		cause         string
		ref           string
		defaultBranch string
		want          string
	}{
		{"push to default branch is shared", "webhook", "main", def, store.CacheScopeShared},
		{"manual run on default branch is shared", "manual", "main", def, store.CacheScopeShared},
		{"upstream fanout on default lane is shared", "upstream", "main", def, store.CacheScopeShared},
		{"schedule on default branch is shared", "schedule", "main", def, store.CacheScopeShared},
		{"poll on default branch is shared", "poll", "main", def, store.CacheScopeShared},
		{"push to feature branch is isolated", "webhook", "feature-x", def, "webhook/feature-x"},
		{"pull_request is isolated by pr lane", "pull_request", "pr:42", def, "pull_request/pr:42"},
		{"pull_request on default-named ref still isolated", "pull_request", "main", def, "pull_request/main"},
		{"merge_group ephemeral queue ref is isolated per group", "merge_group", "gh-readonly-queue/main/pr-7-abc123", def, "merge_group/gh-readonly-queue/main/pr-7-abc123"},
		{"merge_group on default base is isolated, never shared", "merge_group", "main", def, "merge_group/main"},
		{"tag with empty ref is isolated", "tag", "", def, "tag/"},
		{"empty cause defaults to run lane", "", "feature", def, "run/feature"},
		{"no default branch known never shares", "webhook", "main", "", "webhook/main"},
		{"empty ref push never shares (no default match)", "webhook", "", def, "webhook/"},
		// Fail-safe allowlist: a future/unknown cause must NEVER write the
		// shared lane, even when it lands on the default branch.
		{"unknown cause on default branch is isolated", "merge_queue_v2", "main", def, "merge_queue_v2/main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := store.CacheScopeFor(tt.cause, tt.ref, tt.defaultBranch)
			if got != tt.want {
				t.Errorf("CacheScopeFor(%q,%q,%q) = %q, want %q",
					tt.cause, tt.ref, tt.defaultBranch, got, tt.want)
			}
		})
	}
}

// TestCaches_ScopedIsolation proves two lanes for the same key never
// clobber: distinct storage blobs, each GET returns its own lane's
// bytes. This is the whole point — a PR's PUT can't overwrite master's.
func TestCaches_ScopedIsolation(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "scope-iso")

	shared, err := s.UpsertPendingCacheScoped(ctx, projectID, "gradle", store.CacheScopeShared)
	if err != nil {
		t.Fatalf("upsert shared: %v", err)
	}
	pr, err := s.UpsertPendingCacheScoped(ctx, projectID, "gradle", "pull_request/pr:7")
	if err != nil {
		t.Fatalf("upsert pr: %v", err)
	}
	if shared.ID == pr.ID {
		t.Fatalf("lanes collapsed into one row: %s", shared.ID)
	}
	if shared.StorageKey == pr.StorageKey {
		t.Errorf("lanes share a blob path %q — a PR write would clobber the shared cache", shared.StorageKey)
	}
	// Shared lane hashes identically to the legacy key (back-compat).
	if shared.StorageKey != store.CacheStorageKey(projectID, "gradle") {
		t.Errorf("shared lane storage_key = %q, want legacy %q",
			shared.StorageKey, store.CacheStorageKey(projectID, "gradle"))
	}

	if err := s.MarkCacheReady(ctx, shared.ID, 100, "sharedsha"); err != nil {
		t.Fatalf("mark shared: %v", err)
	}
	if err := s.MarkCacheReady(ctx, pr.ID, 200, "prsha"); err != nil {
		t.Fatalf("mark pr: %v", err)
	}

	gotShared, err := s.GetReadyCacheByKeyScoped(ctx, projectID, "gradle", store.CacheScopeShared)
	if err != nil {
		t.Fatalf("get shared: %v", err)
	}
	if gotShared.ContentSHA256 != "sharedsha" || gotShared.SizeBytes != 100 {
		t.Errorf("shared lane returned %+v, want sharedsha/100", gotShared)
	}
	gotPR, err := s.GetReadyCacheByKeyScoped(ctx, projectID, "gradle", "pull_request/pr:7")
	if err != nil {
		t.Fatalf("get pr: %v", err)
	}
	if gotPR.ContentSHA256 != "prsha" || gotPR.SizeBytes != 200 {
		t.Errorf("pr lane returned %+v, want prsha/200", gotPR)
	}
}

// TestCaches_ScopedFallbackToShared proves a ref with no cache of its
// own reads the shared ("@") lane — the GitHub model where a PR's first
// run restores the default-branch cache as a base.
func TestCaches_ScopedFallbackToShared(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "scope-fallback")

	// Only the shared lane is populated.
	shared, _ := s.UpsertPendingCacheScoped(ctx, projectID, "deps", store.CacheScopeShared)
	if err := s.MarkCacheReady(ctx, shared.ID, 512, "base"); err != nil {
		t.Fatalf("mark shared: %v", err)
	}

	// A PR lane with no row of its own falls back to "@".
	got, err := s.GetReadyCacheByKeyScoped(ctx, projectID, "deps", "pull_request/pr:1")
	if err != nil {
		t.Fatalf("pr fallback get: %v", err)
	}
	if got.ContentSHA256 != "base" {
		t.Errorf("fallback returned %q, want the shared 'base' blob", got.ContentSHA256)
	}

	// No shared row + no own row → genuine miss.
	if _, err := s.GetReadyCacheByKeyScoped(ctx, projectID, "absent", "pull_request/pr:1"); !errors.Is(err, store.ErrCacheNotFound) {
		t.Errorf("absent key: err=%v, want ErrCacheNotFound", err)
	}
}

// TestCaches_ScopedFallbackBumpsSharedLane proves a PR reading the
// shared lane keeps it warm against the TTL sweep — so an actively-read
// default-branch cache never ages out from under the PRs that depend on
// it.
func TestCaches_ScopedFallbackBumpsSharedLane(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "scope-bump")

	shared, _ := s.UpsertPendingCacheScoped(ctx, projectID, "deps", store.CacheScopeShared)
	_ = s.MarkCacheReady(ctx, shared.ID, 1, "x")
	// Backdate the shared lane far into the past.
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '40 days' WHERE id = $1`, shared.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// A PR read falls back to "@" and must bump its last_accessed_at.
	if _, err := s.GetReadyCacheByKeyScoped(ctx, projectID, "deps", "pull_request/pr:9"); err != nil {
		t.Fatalf("pr fallback: %v", err)
	}

	// Now the shared lane is fresh — a 30-day sweep must not reap it.
	expired, err := s.ListExpiredCachesScoped(ctx, 30*24*time.Hour, 7*24*time.Hour, 10)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	for _, e := range expired {
		if e.ID == shared.ID {
			t.Errorf("shared lane reaped despite a fresh fallback read")
		}
	}
}

// TestListExpiredCachesScoped_DualTTL proves the isolated lanes age out
// on the shorter horizon while the shared lane keeps the long one: a
// 10-day-idle PR cache expires under a 7-day scoped TTL, an identically
// idle shared cache does not under a 30-day shared TTL.
func TestListExpiredCachesScoped_DualTTL(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "scope-ttl")

	shared, _ := s.UpsertPendingCacheScoped(ctx, projectID, "k", store.CacheScopeShared)
	_ = s.MarkCacheReady(ctx, shared.ID, 1, "a")
	scoped, _ := s.UpsertPendingCacheScoped(ctx, projectID, "k", "webhook/feature")
	_ = s.MarkCacheReady(ctx, scoped.ID, 1, "b")

	// Both idle for 10 days.
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '10 days'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	expired, err := s.ListExpiredCachesScoped(ctx, 30*24*time.Hour, 7*24*time.Hour, 10)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	var sawShared, sawScoped bool
	for _, e := range expired {
		switch e.ID {
		case shared.ID:
			sawShared = true
		case scoped.ID:
			sawScoped = true
		}
	}
	if sawShared {
		t.Errorf("shared lane expired at 10 days under a 30-day TTL")
	}
	if !sawScoped {
		t.Errorf("scoped lane NOT expired at 10 days under a 7-day TTL")
	}
}

// TestListExpiredPendingCaches proves abandoned `pending` lanes are
// reclaimable by updated_at past a grace, while a fresh pending (upload
// still plausibly in flight) and a ready row are left alone. Without
// this the per-ref lanes leak unboundedly when agents die mid-upload.
func TestListExpiredPendingCaches(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "pending-leak")

	stale, _ := s.UpsertPendingCacheScoped(ctx, projectID, "k", "pull_request/pr:1")
	fresh, _ := s.UpsertPendingCacheScoped(ctx, projectID, "k", "pull_request/pr:2")
	ready, _ := s.UpsertPendingCacheScoped(ctx, projectID, "k", store.CacheScopeShared)
	_ = s.MarkCacheReady(ctx, ready.ID, 1, "x")

	// Backdate only the stale pending row's updated_at past the grace.
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET updated_at = NOW() - interval '48 hours' WHERE id = $1`, stale.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	got, err := s.ListExpiredPendingCaches(ctx, 24*time.Hour, 10)
	if err != nil {
		t.Fatalf("list expired pending: %v", err)
	}
	ids := map[uuid.UUID]bool{}
	for _, e := range got {
		ids[e.ID] = true
	}
	if !ids[stale.ID] {
		t.Error("stale pending lane not returned")
	}
	if ids[fresh.ID] {
		t.Error("fresh pending lane returned (upload may still be in flight)")
	}
	if ids[ready.ID] {
		t.Error("ready row returned by the pending sweep")
	}
}

// TestMarkCacheReady_BumpsLastAccessed proves a long-idle row re-uploaded
// to ready restarts its TTL clock, so the sweeper can't evict a
// just-completed cache.
func TestMarkCacheReady_BumpsLastAccessed(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "ready-bump")

	c, _ := s.UpsertPendingCacheScoped(ctx, projectID, "k", store.CacheScopeShared)
	// Simulate a row that has been around (and idle) for ages.
	if _, err := pool.Exec(ctx,
		`UPDATE caches SET last_accessed_at = NOW() - interval '90 days' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if err := s.MarkCacheReady(ctx, c.ID, 1, "x"); err != nil {
		t.Fatalf("mark ready: %v", err)
	}

	// A 30-day TTL sweep must NOT see it — last_accessed_at was bumped.
	expired, err := s.ListExpiredCachesScoped(ctx, 30*24*time.Hour, 7*24*time.Hour, 10)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	for _, e := range expired {
		if e.ID == c.ID {
			t.Error("freshly-ready row evicted — last_accessed_at not bumped on MarkCacheReady")
		}
	}
}

// TestResolveCacheScope exercises the DB glue: a run on the default
// branch resolves to the shared lane, a PR / merge_group / missing-SCM
// run to isolated lanes, and a missing run degrades safely.
func TestResolveCacheScope(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	def := &domain.Pipeline{
		Name: "build",
		Jobs: []domain.Job{{Name: "b", Stage: "s", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}}},
	}
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "resolve-scope", Name: "resolve-scope",
		Pipelines: []*domain.Pipeline{def},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	projectID := applied.ProjectID
	pipelineID := applied.Pipelines[0].PipelineID

	// Seed the SCM source directly (ApplyProject's SCM path needs an
	// auth-provider encryptor the bare test store doesn't wire). Only
	// default_branch matters to ResolveCacheScope.
	if _, err := pool.Exec(ctx, `
		INSERT INTO scm_sources (project_id, provider, url, default_branch)
		VALUES ($1, 'github', 'https://example.com/acme/repo', 'main')
	`, projectID); err != nil {
		t.Fatalf("seed scm: %v", err)
	}

	insertRun := func(t *testing.T, cause, ref string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO runs (id, pipeline_id, counter, cause, status, revisions, triggered_by, ref, definition)
			VALUES ($1, $2, $3, $4, 'queued', '{}'::jsonb, 'system:test', $5, '{}'::jsonb)
		`, id, pipelineID, time.Now().UnixNano(), cause, ref); err != nil {
			t.Fatalf("insert run: %v", err)
		}
		return id
	}

	t.Run("default-branch push → shared", func(t *testing.T) {
		runID := insertRun(t, "webhook", "main")
		got, err := s.ResolveCacheScope(ctx, runID, projectID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got != store.CacheScopeShared {
			t.Errorf("scope = %q, want shared", got)
		}
	})

	t.Run("pull_request → isolated", func(t *testing.T) {
		runID := insertRun(t, "pull_request", "pr:5")
		got, _ := s.ResolveCacheScope(ctx, runID, projectID)
		if got != "pull_request/pr:5" {
			t.Errorf("scope = %q, want pull_request/pr:5", got)
		}
	})

	t.Run("merge_group ephemeral ref → isolated per group, not shared", func(t *testing.T) {
		// Production passes the ephemeral queue head ref, not the base
		// branch (webhook/merge_group.go → merge_group_create.go).
		runID := insertRun(t, "merge_group", "gh-readonly-queue/main/pr-7-abc123")
		got, _ := s.ResolveCacheScope(ctx, runID, projectID)
		if got == store.CacheScopeShared {
			t.Fatalf("merge_group leaked into the shared lane")
		}
		if got != "merge_group/gh-readonly-queue/main/pr-7-abc123" {
			t.Errorf("scope = %q, want per-group lane", got)
		}
	})

	t.Run("missing run → isolated by run id, never shared", func(t *testing.T) {
		missing := uuid.New()
		got, err := s.ResolveCacheScope(ctx, missing, projectID)
		if err != nil {
			t.Fatalf("resolve missing: %v", err)
		}
		if got == store.CacheScopeShared {
			t.Errorf("missing run resolved to the shared lane")
		}
	})
}
