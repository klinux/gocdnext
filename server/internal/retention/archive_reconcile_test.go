package retention_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// recordingSubmitter is what archiveSubmitter sees in the unit test.
// We compare against snapshot() to assert which jobs got re-enqueued
// — letting the test stay deterministic without the full archiver
// machinery wired up.
type recordingSubmitter struct {
	mu  sync.Mutex
	got []uuid.UUID
}

func (r *recordingSubmitter) Submit(jobRunID uuid.UUID) {
	r.mu.Lock()
	r.got = append(r.got, jobRunID)
	r.mu.Unlock()
}

func (r *recordingSubmitter) snapshot() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]uuid.UUID, len(r.got))
	copy(out, r.got)
	return out
}

// seedTerminalJob mirrors logarchive_test's seedJob but flips the
// job to a terminal status (success) and stamps finished_at to a
// time the test pins, so the grace check is deterministic. Returns
// the job_run_id + the project's slug so callers can wire up the
// override flag.
func seedTerminalJob(t *testing.T, pool *pgxpool.Pool, s *store.Store, finishedAt time.Time) (jobID uuid.UUID, slug string) {
	t.Helper()
	ctx := context.Background()
	projectID, pipelineID, runID, stageID, agentID :=
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	jobID = uuid.New()
	slug = fmt.Sprintf("p-%s", uuid.NewString()[:8])

	mustExec(t, pool, ctx,
		`INSERT INTO projects (id, slug, name) VALUES ($1, $2, $3)`,
		projectID, slug, "test")
	mustExec(t, pool, ctx,
		`INSERT INTO pipelines (id, project_id, name, definition)
		 VALUES ($1, $2, 'p', '{}'::jsonb)`,
		pipelineID, projectID)
	mustExec(t, pool, ctx,
		`INSERT INTO runs (id, pipeline_id, counter, status, cause, revisions)
		 VALUES ($1, $2, 1, 'success', 'manual', '{}'::jsonb)`,
		runID, pipelineID)
	mustExec(t, pool, ctx,
		`INSERT INTO stage_runs (id, run_id, name, ordinal, status)
		 VALUES ($1, $2, 'build', 1, 'success')`,
		stageID, runID)
	mustExec(t, pool, ctx,
		`INSERT INTO agents (id, name, token_hash) VALUES ($1, $2, 'h')`,
		agentID, fmt.Sprintf("a-%s", uuid.NewString()[:8]))
	mustExec(t, pool, ctx,
		`INSERT INTO job_runs (id, run_id, stage_run_id, name, status, agent_id,
		 started_at, finished_at, exit_code)
		 VALUES ($1, $2, $3, 'compile', 'success', $4, $5, $5, 0)`,
		jobID, runID, stageID, agentID, finishedAt)

	return jobID, slug
}

// mustExec mirrors the helper inside logarchive's test file —
// duplicated here so each package's test can run independently.
func mustExec(t *testing.T, pool *pgxpool.Pool, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("seed exec %q: %v", sql, err)
	}
}

// TestReconcile_ResubmitsTerminalJobs anchors the happy-path: a job
// past finished_at + grace with no logs_archive_uri must end up on
// the submitter.
func TestReconcile_ResubmitsTerminalJobs(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	// 10 minutes ago — well past the default 5m grace.
	finished := time.Now().Add(-10 * time.Minute)
	jobID, _ := seedTerminalJob(t, pool, s, finished)

	sub := &recordingSubmitter{}
	sw := retention.New(s, nil, silent()).
		WithLogArchive(sub, func(_ *bool) bool { return true })

	stats := sw.SweepOnce(context.Background())
	if stats.ArchivesReSubmitted != 1 {
		t.Fatalf("ResSubmitted = %d, want 1", stats.ArchivesReSubmitted)
	}
	got := sub.snapshot()
	if len(got) != 1 || got[0] != jobID {
		t.Fatalf("submitter saw %v, want [%v]", got, jobID)
	}
}

// TestReconcile_RespectsGrace pins the grace check: a job that
// finished within the grace window must NOT be re-submitted, even
// though its URI is null.
func TestReconcile_RespectsGrace(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	// 30s ago — well inside the default 5m grace.
	finished := time.Now().Add(-30 * time.Second)
	_, _ = seedTerminalJob(t, pool, s, finished)

	sub := &recordingSubmitter{}
	sw := retention.New(s, nil, silent()).
		WithLogArchive(sub, func(_ *bool) bool { return true })

	stats := sw.SweepOnce(context.Background())
	if stats.ArchivesReSubmitted != 0 {
		t.Errorf("ResSubmitted = %d, want 0 (job within grace)", stats.ArchivesReSubmitted)
	}
}

// TestReconcile_RespectsResolverOptOut pins the resolver wiring:
// when the per-project resolver returns false (e.g. global=off, or
// the project explicitly opted out) the job stays untouched.
func TestReconcile_RespectsResolverOptOut(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	finished := time.Now().Add(-10 * time.Minute)
	_, _ = seedTerminalJob(t, pool, s, finished)

	sub := &recordingSubmitter{}
	sw := retention.New(s, nil, silent()).
		WithLogArchive(sub, func(_ *bool) bool { return false })

	stats := sw.SweepOnce(context.Background())
	if stats.ArchivesReSubmitted != 0 {
		t.Errorf("ResSubmitted = %d, want 0 (resolver opted out)", stats.ArchivesReSubmitted)
	}
	if len(sub.snapshot()) != 0 {
		t.Errorf("submitter received jobs despite resolver returning false")
	}
}

// TestReconcile_DeletesOrphanedLogLines covers reconciliation #2.
// Stamp logs_archive_uri AND seed log_lines rows; the sweeper must
// drop the rows because the read path already serves from the
// archive.
func TestReconcile_DeletesOrphanedLogLines(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	finished := time.Now().Add(-1 * time.Hour)
	jobID, _ := seedTerminalJob(t, pool, s, finished)

	// Stamp a fake URI to mark the job as already archived.
	if err := s.MarkJobLogsArchived(ctx, jobID, "logs/fake.gz"); err != nil {
		t.Fatalf("mark archived: %v", err)
	}
	// Seed a couple of orphan rows.
	if err := s.BulkInsertLogLines(ctx, []store.LogLine{
		{JobRunID: jobID, Seq: 1, Stream: "stdout", At: time.Now().UTC(), Text: "orphan a"},
		{JobRunID: jobID, Seq: 2, Stream: "stdout", At: time.Now().UTC(), Text: "orphan b"},
	}); err != nil {
		t.Fatalf("seed orphans: %v", err)
	}

	sub := &recordingSubmitter{}
	sw := retention.New(s, nil, silent()).
		WithLogArchive(sub, func(_ *bool) bool { return true })

	stats := sw.SweepOnce(ctx)
	if stats.ArchiveOrphansDeleted != 1 {
		t.Errorf("OrphansDeleted = %d, want 1", stats.ArchiveOrphansDeleted)
	}
	var remaining int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM log_lines WHERE job_run_id = $1`, jobID,
	).Scan(&remaining)
	if remaining != 0 {
		t.Errorf("log_lines remaining = %d, want 0", remaining)
	}

	// Sub must NOT see jobID — it has a URI already, so the
	// re-submit path correctly skips it.
	for _, id := range sub.snapshot() {
		if id == jobID {
			t.Errorf("URI-stamped job was re-submitted: %v", id)
		}
	}
}
