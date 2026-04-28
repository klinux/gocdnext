package logarchive_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/logarchive"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// memoryBlobs implements artifacts.Store for tests. The Get method
// also satisfies store.LogArchiveSource so the read fallback can
// route through the same fake.
type memoryBlobs struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemoryBlobs() *memoryBlobs {
	return &memoryBlobs{objects: map[string][]byte{}}
}

func (m *memoryBlobs) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
	return int64(len(b)), nil
}

func (m *memoryBlobs) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	b, ok := m.objects[key]
	m.mu.Unlock()
	if !ok {
		return nil, artifacts.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memoryBlobs) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *memoryBlobs) Head(_ context.Context, key string) (int64, error) {
	m.mu.Lock()
	b, ok := m.objects[key]
	m.mu.Unlock()
	if !ok {
		return 0, artifacts.ErrNotFound
	}
	return int64(len(b)), nil
}

func (m *memoryBlobs) SignedPutURL(context.Context, string, time.Duration) (artifacts.SignedURL, error) {
	return artifacts.SignedURL{}, errors.New("memoryBlobs: signed put not supported")
}
func (m *memoryBlobs) SignedGetURL(context.Context, string, time.Duration, ...artifacts.GetOption) (artifacts.SignedURL, error) {
	return artifacts.SignedURL{}, errors.New("memoryBlobs: signed get not supported")
}

// seedJob inserts the chain of project → pipeline → run → stage_run →
// agent → job_run rows the archiver path needs, plus `lineCount` log
// rows for the freshly-created job. Returns the job_run_id.
func seedJob(t *testing.T, pool *pgxpool.Pool, s *store.Store, lineCount int) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	projectID, pipelineID, runID, stageID, jobID, agentID :=
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()

	mustExec(t, pool, ctx,
		`INSERT INTO projects (id, slug, name) VALUES ($1, $2, $3)`,
		projectID, fmt.Sprintf("p-%s", uuid.NewString()[:8]), "test")
	mustExec(t, pool, ctx,
		`INSERT INTO pipelines (id, project_id, name, definition)
		 VALUES ($1, $2, 'p', '{}'::jsonb)`,
		pipelineID, projectID)
	mustExec(t, pool, ctx,
		`INSERT INTO runs (id, pipeline_id, counter, status, cause, revisions)
		 VALUES ($1, $2, 1, 'running', 'manual', '{}'::jsonb)`,
		runID, pipelineID)
	mustExec(t, pool, ctx,
		`INSERT INTO stage_runs (id, run_id, name, ordinal, status)
		 VALUES ($1, $2, 'build', 1, 'running')`,
		stageID, runID)
	mustExec(t, pool, ctx,
		`INSERT INTO agents (id, name, token_hash) VALUES ($1, $2, 'h')`,
		agentID, fmt.Sprintf("a-%s", uuid.NewString()[:8]))
	mustExec(t, pool, ctx,
		`INSERT INTO job_runs (id, run_id, stage_run_id, name, status, agent_id)
		 VALUES ($1, $2, $3, 'compile', 'running', $4)`,
		jobID, runID, stageID, agentID)

	now := time.Now().UTC()
	lines := make([]store.LogLine, 0, lineCount)
	for i := 0; i < lineCount; i++ {
		lines = append(lines, store.LogLine{
			JobRunID: jobID,
			Seq:      int64(i + 1),
			Stream:   "stdout",
			At:       now.Add(time.Duration(i) * time.Millisecond),
			Text:     fmt.Sprintf("line %d", i+1),
		})
	}
	if err := s.BulkInsertLogLines(ctx, lines); err != nil {
		t.Fatalf("seed lines: %v", err)
	}
	return jobID
}

func mustExec(t *testing.T, pool *pgxpool.Pool, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("seed exec %q: %v", sql, err)
	}
}

// TestArchiver_RoundTrip drives Submit → Run → archive landed →
// log_lines deleted → archive readable end-to-end.
func TestArchiver_RoundTrip(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	jobID := seedJob(t, pool, s, 12)

	blobs := newMemoryBlobs()
	a := logarchive.New(s, blobs, nil).WithWorkers(1)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		_ = a.Run(runCtx)
		close(done)
	}()

	a.Submit(jobID)

	// Poll until the archive lands. Should be near-instant; cap at
	// 3s so a stuck test doesn't wedge the suite.
	deadline := time.Now().Add(3 * time.Second)
	var archived bool
	for time.Now().Before(deadline) {
		arch, err := s.GetJobLogArchive(ctx, jobID)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if arch.HasArchive {
			archived = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if !archived {
		t.Fatal("archive never recorded")
	}

	var remaining int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM log_lines WHERE job_run_id = $1`, jobID).Scan(&remaining)
	if remaining != 0 {
		t.Errorf("log_lines remaining = %d, want 0", remaining)
	}

	rc, err := blobs.Get(ctx, "logs/"+jobID.String()+".log.gz")
	if err != nil {
		t.Fatalf("blobs.Get: %v", err)
	}
	got, err := logarchive.ReadArchive(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if len(got) != 12 {
		t.Fatalf("archive line count = %d, want 12", len(got))
	}
	for i, l := range got {
		if l.Seq != int64(i+1) || l.Text != fmt.Sprintf("line %d", i+1) {
			t.Errorf("line %d mismatch: %+v", i, l)
		}
	}

	stats := a.Stats()
	if stats.Archived != 1 {
		t.Errorf("Stats.Archived = %d, want 1", stats.Archived)
	}
}
