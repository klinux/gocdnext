package logarchive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
)

// Defaults sized for a single-node deployment. Workers=4 keeps a
// small fan-out (most jobs ship one archive each in well under a
// second). queueSize=512 absorbs bursts when many jobs terminate
// at once — e.g. a stage with parallel matrix runs all reaching
// success in the same agent dispatch.
const (
	DefaultWorkers   = 4
	DefaultQueueSize = 512
	// archiveOpTimeout caps a single archive op so a stuck S3
	// PUT can't pin a worker forever. 5 minutes is generous for
	// even a 100 MB archive over a slow link.
	archiveOpTimeout = 5 * time.Minute
)

// archiveStore is the slice of *store.Store the archiver actually
// touches. Lifting to an interface lets unit tests drive the
// archiver with an in-memory recorder. The Line shape lives in
// this package so the archiver and the store-side reads don't
// fight over a shared type.
type archiveStore interface {
	JobLogsForArchive(ctx context.Context, jobRunID uuid.UUID) ([]Line, error)
	MarkJobLogsArchived(ctx context.Context, jobRunID uuid.UUID, uri string) error
	DeleteLogLinesByJob(ctx context.Context, jobRunID uuid.UUID) error
}

// Archiver runs the cold-archive pipeline asynchronously. One
// instance per server. Lifecycle:
//
//	a := New(store, blobs, log)
//	go a.Run(ctx)
//	a.Submit(jobRunID)  // non-blocking enqueue
//	...
//	cancel()             // ctx cancel drains the queue and exits
type Archiver struct {
	store     archiveStore
	blobs     artifacts.Store
	log       *slog.Logger
	queue     chan uuid.UUID
	workers   int
	keyPrefix string

	// Stats — exposed for ops dashboards.
	mu       sync.Mutex
	archived int64
	failed   int64
	bytes    int64
}

// New wires an Archiver. nil blobs is a programming error — the
// caller is expected to gate creation on EffectiveLogArchive.
func New(s archiveStore, blobs artifacts.Store, log *slog.Logger) *Archiver {
	if log == nil {
		log = slog.Default()
	}
	return &Archiver{
		store:     s,
		blobs:     blobs,
		log:       log,
		queue:     make(chan uuid.UUID, DefaultQueueSize),
		workers:   DefaultWorkers,
		keyPrefix: "logs/",
	}
}

// WithWorkers sets the worker pool size. Must be > 0.
func (a *Archiver) WithWorkers(n int) *Archiver {
	if n > 0 {
		a.workers = n
	}
	return a
}

// WithKeyPrefix overrides the storage-key prefix. Default "logs/".
// Useful when the artifact backend is shared with cache + artifacts
// and operators want a separate namespace for compliance dumps.
func (a *Archiver) WithKeyPrefix(p string) *Archiver {
	a.keyPrefix = p
	return a
}

// Submit enqueues a job for archiving. Non-blocking — drops with a
// warn log if the queue is saturated, since a backlog usually
// indicates upstream failure (object store down) and blocking the
// scheduler on it would compound the outage. The next archive
// attempt for the same job is via the catch-up sweeper (future
// slice) or a manual /admin re-archive.
func (a *Archiver) Submit(jobRunID uuid.UUID) {
	select {
	case a.queue <- jobRunID:
	default:
		a.log.Warn("logarchive: queue full, dropping submit",
			"job_run_id", jobRunID)
	}
}

// Run blocks until ctx is cancelled. Spawns `workers` goroutines
// that pull from the queue. A pool ahead of the queue lets several
// archives run concurrently — limited so the artifact backend
// isn't hammered.
func (a *Archiver) Run(ctx context.Context) error {
	a.log.Info("logarchive: started",
		"workers", a.workers, "queue_size", cap(a.queue))
	var wg sync.WaitGroup
	wg.Add(a.workers)
	for i := 0; i < a.workers; i++ {
		go func() {
			defer wg.Done()
			a.worker(ctx)
		}()
	}
	wg.Wait()
	a.log.Info("logarchive: stopped", "archived", a.archived, "failed", a.failed)
	return nil
}

func (a *Archiver) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-a.queue:
			if !ok {
				return
			}
			a.archiveOne(ctx, id)
		}
	}
}

// archiveOne is the unit of work: read all log_lines for the job,
// gzip them into the archive format, upload, stamp the URI on the
// job_run, drop the rows. Failures at any stage leave the row in
// place so a retry on the next sweeper tick (slice 4) or a manual
// kick can complete the cycle without losing data.
func (a *Archiver) archiveOne(parent context.Context, jobRunID uuid.UUID) {
	ctx, cancel := context.WithTimeout(parent, archiveOpTimeout)
	defer cancel()

	lines, err := a.store.JobLogsForArchive(ctx, jobRunID)
	if err != nil {
		a.fail("read log lines", jobRunID, err)
		return
	}
	if len(lines) == 0 {
		// Empty job — still mark as archived (URI = "" stays NULL
		// in DB) so the read path doesn't keep falling through to
		// log_lines for a job that genuinely has nothing.
		a.log.Debug("logarchive: empty job, skipping upload", "job_run_id", jobRunID)
		return
	}

	var buf bytes.Buffer
	written, err := WriteArchive(&buf, lines)
	if err != nil {
		a.fail("write archive", jobRunID, err)
		return
	}
	key := a.keyPrefix + jobRunID.String() + ".log.gz"
	if _, err := a.blobs.Put(ctx, key, &buf); err != nil {
		a.fail("upload archive", jobRunID, err)
		return
	}
	if err := a.store.MarkJobLogsArchived(ctx, jobRunID, key); err != nil {
		// URI stamp failed AFTER the upload succeeded. Best-effort
		// delete of the orphaned blob — leaving it would leak
		// storage, and the archiver will retry the whole flow on
		// the next pass with a fresh PUT.
		_ = a.blobs.Delete(ctx, key)
		a.fail("mark archived", jobRunID, err)
		return
	}
	if err := a.store.DeleteLogLinesByJob(ctx, jobRunID); err != nil {
		// URI is stamped but rows remain. The read path now serves
		// from the archive; the DB rows are orphaned cost. A future
		// sweeper pass can reconcile by re-running DELETE for any
		// job_run whose URI is set AND log_lines still exist.
		a.log.Warn("logarchive: delete log_lines failed (orphaned rows)",
			"job_run_id", jobRunID, "err", err)
	}

	a.mu.Lock()
	a.archived++
	a.bytes += written
	a.mu.Unlock()
	a.log.Info("logarchive: archived",
		"job_run_id", jobRunID, "lines", len(lines), "bytes", written)
}

func (a *Archiver) fail(stage string, jobRunID uuid.UUID, err error) {
	if errors.Is(err, context.Canceled) {
		// Shutdown — quiet exit.
		return
	}
	a.log.Warn("logarchive: "+stage+" failed",
		"job_run_id", jobRunID, "err", err)
	a.mu.Lock()
	a.failed++
	a.mu.Unlock()
}

// Stats snapshots the running counters. Operators read this via
// admin endpoints; the lock keeps a Prometheus scrape from racing
// with worker increments.
type Stats struct {
	Archived int64
	Failed   int64
	Bytes    int64
}

func (a *Archiver) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Stats{Archived: a.archived, Failed: a.failed, Bytes: a.bytes}
}

// EmptyJobErr is returned by JobLogsForArchive shims that prefer
// an explicit signal over `len([]) == 0`. The archiver itself
// treats `len == 0` as "skip" so this is informational.
var EmptyJobErr = fmt.Errorf("logarchive: no log lines for job")
