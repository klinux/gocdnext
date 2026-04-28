// Package retention runs background processes that reclaim storage
// space from artefacts that have passed their retention policy.
//
// The sweeper ticks on a schedule, claims a bounded batch of artefacts
// the DB considers expired, deletes each blob from the configured
// storage backend, and then removes the DB row. It's safe to run
// multiple instances concurrently — `FOR UPDATE SKIP LOCKED` in the
// claim query partitions work.
//
// Layers implemented in this slice:
//   - TTL: rows with expires_at in the past.
//   - Retry: rows stuck in 'deleting' for longer than the grace
//     window (sweeper crashed between storage-delete and row-delete).
//
// Layers deferred to E2d.2.b: keep-last-N per pipeline, per-project
// soft quota, global hard quota.
package retention

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Defaults picked conservatively: a 10-minute tick puts the p95
// retention drift at under 10 minutes, which is fine for byte-budget
// purposes; 500 per batch keeps the backend delete bounded on S3's
// 1000/request cap; 5-minute grace lets a normal tick finish before a
// retry starts stealing work.
const (
	DefaultTick              = 10 * time.Minute
	DefaultBatchSize         = 500
	DefaultGraceMinutes      = 5
	DefaultKeepLast          = 30
	DefaultProjectQuotaBytes = 100 * 1024 * 1024 * 1024 // 100 GiB
	DefaultGlobalQuotaBytes  = 0                        // 0 = disabled
	// DefaultCacheTTL: 30 days is long enough for weekly builds to
	// keep their cache warm, short enough that abandoned projects
	// surrender disk within a month.
	DefaultCacheTTL = 30 * 24 * time.Hour
	// DefaultCacheProjectQuotaBytes: 0 = disabled. Quotas are
	// opt-in because a sensible default needs real-world data
	// (how big does a pnpm-store tree really get? a Go module
	// cache? a gradle cache?). Operators who care set it; the
	// rest rely on TTL.
	DefaultCacheProjectQuotaBytes = 0
	// DefaultCacheGlobalQuotaBytes: 0 = disabled. Same default as
	// artifact global quota — only multi-tenant deployments with
	// shared disk tend to care, and a one-size-fits-all number
	// would be fiction.
	DefaultCacheGlobalQuotaBytes = 0
	// DefaultLogRetention: 0 = no automatic drop, partitions
	// accumulate until an operator dials this in. Conservative on
	// purpose — log retention is a policy decision (compliance,
	// debugging windows) the platform shouldn't pick blindly.
	DefaultLogRetention = 0 * time.Hour
	// DefaultLogMonthsAhead: keep 3 months of partitions stocked so
	// an agent streaming logs at midnight on the last day of a
	// month never finds an unmaterialised range. Daily ticks top
	// it back up.
	DefaultLogMonthsAhead = 3
	// DefaultArchiveGrace: sweeper waits this long after a job's
	// finished_at before re-enqueueing for archive. Long enough
	// that a slow in-flight Submit completes; short enough that a
	// dropped one is recovered within the same hour.
	DefaultArchiveGrace = 5 * time.Minute
	// DefaultArchiveBatch: bounds the per-tick re-submit count so
	// a backlog can't flood the archiver's queue in one go.
	DefaultArchiveBatch = 100
)

// Sweeper is the long-running task. Call Run inside a goroutine; it
// blocks until ctx is cancelled.
type Sweeper struct {
	store   *store.Store
	storage artifacts.Store
	log     *slog.Logger

	tick         time.Duration
	batchSize    int
	graceMinutes int

	keepLast               int
	projectQuotaBytes      int64
	globalQuotaBytes       int64
	cacheTTL               time.Duration
	cacheProjectQuotaBytes int64
	cacheGlobalQuotaBytes  int64

	// log_lines partition lifecycle. logRetention 0 disables the
	// drop pass (ensure pass always runs — without it, the next
	// agent INSERT in a fresh month has no partition to land in).
	logRetention   time.Duration
	logMonthsAhead int

	// Cold-archive reconciliation. When wired (WithLogArchive*),
	// every tick re-submits terminal jobs that the agent-side hook
	// missed and re-runs DELETE for jobs whose URI is stamped but
	// log_lines rows linger. Nil archiver = pass disabled.
	archiver        archiveSubmitter
	archiveResolver archivePolicyResolver
	archiveGrace    time.Duration
	archiveBatch    int32

	mu          sync.Mutex
	lastStats   SweepStats
	lastSweepAt time.Time
}

// archiveSubmitter is the slice of *logarchive.Archiver the sweeper
// touches — defined here as an interface so the retention package
// doesn't take a hard dep on internal/logarchive's whole surface.
type archiveSubmitter interface {
	Submit(jobRunID uuid.UUID)
}

// archivePolicyResolver decides whether a specific job should be
// archived based on the live global+per-project policy. Pulled into
// an interface for the same reason as archiveSubmitter — and so
// tests can inject a deterministic resolver.
type archivePolicyResolver func(projectFlag *bool) bool

// New wires a Sweeper. nil Store is a programming error (panic via
// use). nil storage is a soft disable — Run() logs + exits early so
// deployments without artefact backend don't spam errors.
func New(s *store.Store, storage artifacts.Store, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{
		store:                  s,
		storage:                storage,
		log:                    log,
		tick:                   DefaultTick,
		batchSize:              DefaultBatchSize,
		graceMinutes:           DefaultGraceMinutes,
		keepLast:               DefaultKeepLast,
		projectQuotaBytes:      DefaultProjectQuotaBytes,
		globalQuotaBytes:       DefaultGlobalQuotaBytes,
		cacheTTL:               DefaultCacheTTL,
		cacheProjectQuotaBytes: DefaultCacheProjectQuotaBytes,
		cacheGlobalQuotaBytes:  DefaultCacheGlobalQuotaBytes,
		logRetention:           DefaultLogRetention,
		logMonthsAhead:         DefaultLogMonthsAhead,
		archiveGrace:           DefaultArchiveGrace,
		archiveBatch:           DefaultArchiveBatch,
	}
}

// WithCacheGlobalQuotaBytes sets the global cache size cap
// across every project. 0 disables. When the total `ready`
// cache bytes exceed this limit, the sweeper evicts oldest-
// accessed rows globally (LRU across projects) until under. Runs
// AFTER per-project quota so a single tenant hogging everything
// loses their own caches before the pain spreads to neighbours.
func (s *Sweeper) WithCacheGlobalQuotaBytes(b int64) *Sweeper {
	if b >= 0 {
		s.cacheGlobalQuotaBytes = b
	}
	return s
}

// WithCacheProjectQuotaBytes sets the per-project cache size cap.
// 0 disables — TTL alone governs eviction. When a project's
// `ready` cache total exceeds this limit, the sweeper deletes
// oldest-accessed rows until the project is back under quota.
func (s *Sweeper) WithCacheProjectQuotaBytes(b int64) *Sweeper {
	if b >= 0 {
		s.cacheProjectQuotaBytes = b
	}
	return s
}

// WithCacheTTL overrides the cache eviction window. 0 disables the
// cache sweep entirely — for deployments that want to keep caches
// forever (tiny project, generous disk). Any positive duration is
// accepted; operator discretion.
func (s *Sweeper) WithCacheTTL(d time.Duration) *Sweeper {
	if d >= 0 {
		s.cacheTTL = d
	}
	return s
}

// WithKeepLast overrides the per-pipeline keep-last-N policy. 0
// disables (no runs are demoted for being "too old-ranked").
func (s *Sweeper) WithKeepLast(n int) *Sweeper {
	if n >= 0 {
		s.keepLast = n
	}
	return s
}

// WithProjectQuotaBytes sets the per-project soft cap. 0 disables.
func (s *Sweeper) WithProjectQuotaBytes(b int64) *Sweeper {
	if b >= 0 {
		s.projectQuotaBytes = b
	}
	return s
}

// WithGlobalQuotaBytes sets the global hard cap. 0 disables.
func (s *Sweeper) WithGlobalQuotaBytes(b int64) *Sweeper {
	if b >= 0 {
		s.globalQuotaBytes = b
	}
	return s
}

// WithLogRetention sets the maximum age for log_lines partitions.
// 0 disables automatic drops — partitions still get created ahead
// of time, but old ones survive until manual cleanup. Anything
// positive activates the drop pass; partitions whose upper bound
// falls before now-d are dropped on the next tick.
func (s *Sweeper) WithLogRetention(d time.Duration) *Sweeper {
	if d >= 0 {
		s.logRetention = d
	}
	return s
}

// WithLogMonthsAhead sets how many future months of partitions the
// ensure pass keeps stocked. Default 3 — daily ticks refresh, so 3
// is enough cushion to survive a multi-day outage.
func (s *Sweeper) WithLogMonthsAhead(n int) *Sweeper {
	if n >= 0 {
		s.logMonthsAhead = n
	}
	return s
}

// WithLogArchive enables the cold-archive reconciliation pass. The
// sweeper re-submits terminal jobs that the agent-side hook missed
// and runs DELETE for jobs whose URI is stamped but log_lines rows
// linger. Pass nil submitter or nil resolver to disable.
func (s *Sweeper) WithLogArchive(submitter archiveSubmitter, resolver archivePolicyResolver) *Sweeper {
	s.archiver = submitter
	s.archiveResolver = resolver
	return s
}

// WithArchiveGrace overrides the grace window after a job's
// finished_at before the sweeper re-enqueues it for archive.
// Default 5 minutes.
func (s *Sweeper) WithArchiveGrace(d time.Duration) *Sweeper {
	if d > 0 {
		s.archiveGrace = d
	}
	return s
}

// WithArchiveBatch caps how many jobs the sweeper re-submits or
// orphan-deletes per tick. Default 100.
func (s *Sweeper) WithArchiveBatch(n int32) *Sweeper {
	if n > 0 {
		s.archiveBatch = n
	}
	return s
}

// WithTick overrides the tick interval. Mainly for tests.
func (s *Sweeper) WithTick(d time.Duration) *Sweeper {
	if d > 0 {
		s.tick = d
	}
	return s
}

// WithBatchSize overrides the per-tick cap.
func (s *Sweeper) WithBatchSize(n int) *Sweeper {
	if n > 0 {
		s.batchSize = n
	}
	return s
}

// WithGraceMinutes overrides the retry grace for rows stuck in
// 'deleting' (how long we wait before assuming a previous sweeper
// attempt crashed and re-claiming the row).
func (s *Sweeper) WithGraceMinutes(n int) *Sweeper {
	if n > 0 {
		s.graceMinutes = n
	}
	return s
}

// Run blocks until ctx is cancelled. Runs one sweep immediately, then
// on each tick. Storage is optional — the artifact eviction pass
// short-circuits when it's nil, but log-partition lifecycle still
// runs (no point letting partitions go unmaterialised just because
// no artefact backend is wired).
func (s *Sweeper) Run(ctx context.Context) error {
	if s.storage == nil {
		s.log.Info("retention: no artifact backend configured; only log-partition lifecycle will run")
	}
	s.log.Info("retention: sweeper started",
		"tick", s.tick, "batch", s.batchSize, "grace_minutes", s.graceMinutes,
		"log_retention", s.logRetention, "log_months_ahead", s.logMonthsAhead)

	// First sweep on start so ops can boot with a pending backlog and
	// see progress immediately.
	s.SweepOnce(ctx)

	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.SweepOnce(ctx)
		}
	}
}

// SweepStats is what one tick produced. Exposed so ops/metrics can
// log or export it; the sweeper itself only logs aggregate numbers.
type SweepStats struct {
	// Demotions: rows that had their expires_at bumped to NOW by the
	// keep-last / project-quota / global-quota layers. They become
	// delete candidates on the SAME tick (the TTL claim below).
	DemotedKeepLast   int64
	DemotedProjectCap int64
	DemotedGlobalCap  int64

	// Actual delete pass.
	Claimed         int
	Deleted         int
	StorageFailures int
	DBFailures      int
	BytesFreed      int64

	// Log partition lifecycle (also same tick).
	LogPartitionsCreated int
	LogPartitionsDropped int

	// Cold-archive reconciliation. ReSubmitted = terminal jobs
	// pushed back into the archiver queue. OrphansDeleted = jobs
	// whose URI was stamped but log_lines rows were still around;
	// the rows have been dropped on this tick.
	ArchivesReSubmitted int
	ArchiveOrphansDeleted int

	// Cache sweep (piggybacks on the same tick — no separate
	// sweeper goroutine). Expired caches are ready rows whose
	// last_accessed_at fell past the cache TTL. Quota-evicted
	// rows are counted separately so ops can tell "abandoned
	// caches" apart from "active project pushed past quota".
	CachesDeleted              int
	CachesDeletedByQuota       int
	CachesDeletedByGlobalQuota int
	CacheStorageFailures       int
	CacheDBFailures            int
	CacheBytesFreed            int64
}

// SweepOnce runs a single batch and returns what happened. Exported so
// tests can drive the sweep without a ticker.
//
// Order matters: keep-last → project cap → global cap → TTL. The
// earlier layers only stamp expires_at=NOW; the final TTL claim is
// what removes the object + row. Running them in the same tick means
// a fresh demotion gets reaped in the SAME pass, not ten minutes
// later.
func (s *Sweeper) SweepOnce(ctx context.Context) SweepStats {
	var stats SweepStats

	// Log-partition lifecycle: runs first, even when no artefact
	// store is wired. Cheap (1 EXISTS + at most a few CREATE/DROP
	// per tick) and prevents agent INSERT failures at month flip.
	logStats := SweepLogPartitions(ctx, s.store, time.Now(),
		s.logMonthsAhead, s.logRetention, s.log)
	stats.LogPartitionsCreated = logStats.Created
	stats.LogPartitionsDropped = logStats.Dropped

	// Cold-archive reconciliation. Independent of artefact-store
	// gating below — the archiver may be wired even when keep-last
	// quotas etc. aren't.
	if s.archiver != nil && s.archiveResolver != nil {
		s.reconcileArchives(ctx, &stats)
	}

	// Artefact retention from here down depends on the storage
	// backend; skip when not configured. Stats from the log-only
	// pass are still latched into lastStats so /admin/retention
	// reflects what happened.
	if s.storage == nil {
		s.mu.Lock()
		s.lastStats = stats
		s.lastSweepAt = time.Now()
		s.mu.Unlock()
		return stats
	}

	if s.keepLast > 0 {
		n, err := s.store.ExpireArtifactsBeyondKeepLast(ctx, s.keepLast)
		if err != nil {
			s.log.Warn("retention: keep-last expire", "err", err)
		}
		stats.DemotedKeepLast = n
	}

	if s.projectQuotaBytes > 0 {
		over, err := s.store.ListProjectsOverArtifactQuota(ctx, s.projectQuotaBytes)
		if err != nil {
			s.log.Warn("retention: list over-quota projects", "err", err)
		}
		for _, p := range over {
			excess := p.Bytes - s.projectQuotaBytes
			n, err := s.store.ExpireOldestInProjectByExcess(ctx, p.ProjectID, excess)
			if err != nil {
				s.log.Warn("retention: project quota expire",
					"project_id", p.ProjectID, "err", err)
				continue
			}
			stats.DemotedProjectCap += n
			s.log.Info("retention: project over quota",
				"project_id", p.ProjectID, "bytes", p.Bytes, "quota", s.projectQuotaBytes, "demoted", n)
		}
	}

	if s.globalQuotaBytes > 0 {
		total, err := s.store.GlobalArtifactUsage(ctx)
		if err != nil {
			s.log.Warn("retention: global usage", "err", err)
		} else if total > s.globalQuotaBytes {
			excess := total - s.globalQuotaBytes
			n, err := s.store.ExpireOldestGloballyByExcess(ctx, excess)
			if err != nil {
				s.log.Warn("retention: global quota expire", "err", err)
			} else {
				stats.DemotedGlobalCap = n
				s.log.Info("retention: global over quota",
					"bytes", total, "quota", s.globalQuotaBytes, "demoted", n)
			}
		}
	}

	claimed, err := s.store.ClaimArtifactsForSweep(ctx, s.batchSize, s.graceMinutes)
	if err != nil {
		s.log.Warn("retention: claim failed", "err", err)
		// Don't early-return: cache eviction + auth hygiene still
		// need to run on this tick even when the artifact claim
		// itself failed or returned zero rows.
	}
	stats.Claimed = len(claimed)

	for _, row := range claimed {
		if err := s.storage.Delete(ctx, row.StorageKey); err != nil {
			if !errors.Is(err, artifacts.ErrNotFound) {
				s.log.Warn("retention: storage delete failed",
					"storage_key", row.StorageKey, "err", err)
				stats.StorageFailures++
				// Leave the row in 'deleting' — next tick after the
				// grace window retries it. Don't delete the DB row
				// because the object is still out there.
				continue
			}
			// ErrNotFound is fine: idempotent delete. Row can be
			// reaped.
		}
		if err := s.store.RemoveArtifactRow(ctx, row.ID); err != nil {
			s.log.Warn("retention: remove row failed",
				"artifact_id", row.ID, "err", err)
			stats.DBFailures++
			continue
		}
		stats.Deleted++
		stats.BytesFreed += row.SizeBytes
	}

	// Cache eviction runs on the same tick so a single batch cap
	// bounds the whole sweep. Simple model: fetch expired rows,
	// delete blob, delete row. No "deleting" intermediate state —
	// if the blob delete fails the row stays and next tick tries
	// again; if the row delete fails the blob is gone but a
	// subsequent fetch returns 404 (which the agent already treats
	// as a miss). Self-healing either way.
	if s.cacheTTL > 0 {
		expired, err := s.store.ListExpiredCaches(ctx, s.cacheTTL, s.batchSize)
		if err != nil {
			s.log.Warn("retention: list expired caches", "err", err)
		}
		s.deleteCaches(ctx, expired, &stats, cacheEvictTTL)
	}

	// Per-project quota runs AFTER TTL: a TTL pass may have
	// already brought the project back under quota, so
	// re-computing usage here avoids over-eviction. LRU across
	// the project's remaining `ready` rows until enough bytes
	// are reclaimed to fall under the limit.
	if s.cacheProjectQuotaBytes > 0 {
		over, err := s.store.ListProjectsOverCacheQuota(ctx, s.cacheProjectQuotaBytes)
		if err != nil {
			s.log.Warn("retention: list over-cache-quota projects", "err", err)
		}
		for _, p := range over {
			excess := p.Bytes - s.cacheProjectQuotaBytes
			candidates, err := s.store.ListOldestCachesInProject(ctx, p.ProjectID, s.batchSize)
			if err != nil {
				s.log.Warn("retention: list oldest caches",
					"project_id", p.ProjectID, "err", err)
				continue
			}
			// Take the minimal prefix whose sizes sum to `excess`.
			// Picking the full `candidates` list would trim more
			// than needed and hurt the next build's hit rate.
			var toDelete []store.ExpiredCache
			var freed int64
			for _, c := range candidates {
				if freed >= excess {
					break
				}
				toDelete = append(toDelete, c)
				freed += c.SizeBytes
			}
			s.log.Info("retention: cache project over quota",
				"project_id", p.ProjectID, "bytes", p.Bytes,
				"quota", s.cacheProjectQuotaBytes, "targeted", len(toDelete))
			s.deleteCaches(ctx, toDelete, &stats, cacheEvictProjectQuota)
		}
	}

	// Global cache quota runs last so per-project quota had a
	// chance to bring the worst offenders under their own caps
	// first. LRU across the whole table — the oldest idle key
	// from any project goes before a fresh hit from another.
	if s.cacheGlobalQuotaBytes > 0 {
		total, err := s.store.GlobalCacheUsage(ctx)
		if err != nil {
			s.log.Warn("retention: global cache usage", "err", err)
		} else if total > s.cacheGlobalQuotaBytes {
			excess := total - s.cacheGlobalQuotaBytes
			candidates, err := s.store.ListOldestCachesGlobally(ctx, s.batchSize)
			if err != nil {
				s.log.Warn("retention: list oldest caches globally", "err", err)
			} else {
				var toDelete []store.ExpiredCache
				var freed int64
				for _, c := range candidates {
					if freed >= excess {
						break
					}
					toDelete = append(toDelete, c)
					freed += c.SizeBytes
				}
				s.log.Info("retention: cache global over quota",
					"bytes", total, "quota", s.cacheGlobalQuotaBytes, "targeted", len(toDelete))
				s.deleteCaches(ctx, toDelete, &stats, cacheEvictGlobalQuota)
			}
		}
	}

	// Auth hygiene: expired sessions + OAuth state rows aren't part
	// of the artifact pipeline, but they accumulate in the same DB
	// and we already have a goroutine ticking — piggyback so ops
	// don't need a second sweeper. Failures are warnings, not
	// fatal: the next tick retries.
	if err := s.store.SweepAuthStates(ctx); err != nil {
		s.log.Warn("retention: sweep auth states", "err", err)
	}
	if err := s.store.SweepUserSessions(ctx); err != nil {
		s.log.Warn("retention: sweep user sessions", "err", err)
	}

	s.log.Info("retention: sweep done",
		"demoted_keep_last", stats.DemotedKeepLast,
		"demoted_project_cap", stats.DemotedProjectCap,
		"demoted_global_cap", stats.DemotedGlobalCap,
		"claimed", stats.Claimed,
		"deleted", stats.Deleted,
		"bytes_freed", stats.BytesFreed,
		"storage_failures", stats.StorageFailures,
		"db_failures", stats.DBFailures,
		"caches_deleted", stats.CachesDeleted,
		"caches_deleted_by_quota", stats.CachesDeletedByQuota,
		"caches_deleted_by_global_quota", stats.CachesDeletedByGlobalQuota,
		"cache_bytes_freed", stats.CacheBytesFreed,
		"cache_storage_failures", stats.CacheStorageFailures,
		"cache_db_failures", stats.CacheDBFailures)

	s.mu.Lock()
	s.lastStats = stats
	s.lastSweepAt = time.Now()
	s.mu.Unlock()
	return stats
}

// cacheEvictionKind labels which pass triggered a delete so the
// stats + logs stay legible. TTL (time-based) and the two quota
// passes need distinct counters so ops can tell "abandoned
// caches ageing out" from "active project hit the ceiling".
type cacheEvictionKind int

const (
	cacheEvictTTL cacheEvictionKind = iota
	cacheEvictProjectQuota
	cacheEvictGlobalQuota
)

func (k cacheEvictionKind) String() string {
	switch k {
	case cacheEvictProjectQuota:
		return "project_quota"
	case cacheEvictGlobalQuota:
		return "global_quota"
	default:
		return "ttl"
	}
}

// deleteCaches is the shared blob+row delete loop behind every
// cache eviction path. The kind flag routes successes into the
// right stats counter; bytes freed and failures aggregate into
// the same fields regardless of which pass triggered the call.
func (s *Sweeper) deleteCaches(ctx context.Context, rows []store.ExpiredCache, stats *SweepStats, kind cacheEvictionKind) {
	for _, c := range rows {
		if err := s.storage.Delete(ctx, c.StorageKey); err != nil {
			if !errors.Is(err, artifacts.ErrNotFound) {
				s.log.Warn("retention: cache storage delete failed",
					"storage_key", c.StorageKey, "err", err, "kind", kind.String())
				stats.CacheStorageFailures++
				continue
			}
		}
		if err := s.store.DeleteCacheRow(ctx, c.ID); err != nil {
			s.log.Warn("retention: cache row delete failed",
				"cache_id", c.ID, "err", err, "kind", kind.String())
			stats.CacheDBFailures++
			continue
		}
		switch kind {
		case cacheEvictProjectQuota:
			stats.CachesDeletedByQuota++
		case cacheEvictGlobalQuota:
			stats.CachesDeletedByGlobalQuota++
		default:
			stats.CachesDeleted++
		}
		stats.CacheBytesFreed += c.SizeBytes
	}
}

// Snapshot is the admin-page view of what the sweeper is configured
// to do and what the last tick produced. Zero LastSweepAt means the
// sweeper hasn't ticked yet (fresh boot or storage-disabled).
type Snapshot struct {
	Enabled                bool          `json:"enabled"`
	Tick                   time.Duration `json:"tick"`
	BatchSize              int           `json:"batch_size"`
	GraceMinutes           int           `json:"grace_minutes"`
	KeepLast               int           `json:"keep_last"`
	ProjectQuotaBytes      int64         `json:"project_quota_bytes"`
	GlobalQuotaBytes       int64         `json:"global_quota_bytes"`
	CacheTTL               time.Duration `json:"cache_ttl"`
	CacheProjectQuotaBytes int64         `json:"cache_project_quota_bytes"`
	CacheGlobalQuotaBytes  int64         `json:"cache_global_quota_bytes"`
	LastSweepAt            time.Time     `json:"last_sweep_at,omitempty"`
	Last                   SweepStats    `json:"last_stats"`
}

// Snapshot returns the current config + the last tick's stats. Safe
// to call from an HTTP handler concurrently with Run.
func (s *Sweeper) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{
		Enabled:                s.storage != nil,
		Tick:                   s.tick,
		BatchSize:              s.batchSize,
		GraceMinutes:           s.graceMinutes,
		KeepLast:               s.keepLast,
		ProjectQuotaBytes:      s.projectQuotaBytes,
		GlobalQuotaBytes:       s.globalQuotaBytes,
		CacheTTL:               s.cacheTTL,
		CacheProjectQuotaBytes: s.cacheProjectQuotaBytes,
		CacheGlobalQuotaBytes:  s.cacheGlobalQuotaBytes,
		LastSweepAt:            s.lastSweepAt,
		Last:                   s.lastStats,
	}
}
