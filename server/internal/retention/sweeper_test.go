package retention_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// fakeStore captures the backend-side delete calls so tests can assert
// against them without a real filesystem/S3/GCS round-trip.
type fakeStore struct {
	mu      sync.Mutex
	deleted map[string]int
	failOn  map[string]error
}

func newFakeStore() *fakeStore {
	return &fakeStore{deleted: map[string]int{}, failOn: map[string]error{}}
}

func (f *fakeStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted[key]++
	if err, ok := f.failOn[key]; ok {
		return err
	}
	return nil
}

// Implement the rest of artifacts.Store with stubs — sweeper only
// calls Delete, but the interface forces us to satisfy the surface.
func (f *fakeStore) SignedPutURL(context.Context, string, time.Duration) (artifacts.SignedURL, error) {
	return artifacts.SignedURL{}, errors.New("fakeStore: not used")
}
func (f *fakeStore) SignedGetURL(context.Context, string, time.Duration) (artifacts.SignedURL, error) {
	return artifacts.SignedURL{}, errors.New("fakeStore: not used")
}
func (f *fakeStore) Head(context.Context, string) (int64, error) {
	return 0, errors.New("fakeStore: not used")
}
func (f *fakeStore) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, errors.New("fakeStore: not used")
}
func (f *fakeStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("fakeStore: not used")
}

func (f *fakeStore) deleteCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleted[key]
}

// seedArtifact inserts one artefact with a concrete expires_at.
// Reuses the dispatch-level seedPipeline indirectly through a run.
func seedArtifact(t *testing.T, pool *pgxpool.Pool, key string, expiresAt time.Time, status string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	s := store.New(pool)

	fp := domain.GitFingerprint("https://github.com/org/ret", "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "ret-" + key[:8], Name: "ret",
		Pipelines: []*domain.Pipeline{{
			Name:   "p1",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/ret", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applied.Pipelines[0].PipelineID

	var materialID uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID)

	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID,
		ModificationID: 1,
		Revision:       "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1",
		Branch:         "main", Provider: "github", Delivery: "t", TriggeredBy: "test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	jobID := res.JobRuns[0].ID

	row, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID: res.RunID, JobRunID: jobID,
		PipelineID: pipelineID, ProjectID: applied.ProjectID,
		Path: "bin/x", StorageKey: key,
		ExpiresAt: &expiresAt,
	})
	if err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	switch status {
	case "ready":
		if _, err := s.MarkArtifactReady(ctx, key, 512, "deadbeef"); err != nil {
			t.Fatalf("mark ready: %v", err)
		}
	case "deleting":
		// Simulate a prior sweeper crash mid-delete: row in 'deleting'
		// with an old deleted_at so it's past the grace window.
		if _, err := pool.Exec(ctx,
			`UPDATE artifacts SET status='deleting', deleted_at = NOW() - INTERVAL '1 hour' WHERE id = $1`,
			row.ID,
		); err != nil {
			t.Fatalf("patch deleting: %v", err)
		}
	case "pending":
		// default state, no patch
	}
	return row.ID
}

func silent() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSweeper_TTLExpiredReady_IsDeletedFromStorageAndDB(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()

	key := uuid.NewString()
	seedArtifact(t, pool, key, time.Now().Add(-time.Hour), "ready")

	sw := retention.New(s, fs, silent())
	stats := sw.SweepOnce(context.Background())

	if stats.Claimed != 1 || stats.Deleted != 1 {
		t.Errorf("stats = %+v", stats)
	}
	if fs.deleteCount(key) != 1 {
		t.Errorf("storage delete count = %d", fs.deleteCount(key))
	}
	// Row gone from DB.
	_, err := s.GetArtifactByStorageKey(context.Background(), key)
	if !errors.Is(err, store.ErrArtifactNotFound) {
		t.Errorf("want ErrArtifactNotFound, got %v", err)
	}
}

func TestSweeper_TTLNotYetExpired_IsKept(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()

	key := uuid.NewString()
	seedArtifact(t, pool, key, time.Now().Add(24*time.Hour), "ready")

	sw := retention.New(s, fs, silent())
	stats := sw.SweepOnce(context.Background())

	if stats.Claimed != 0 || stats.Deleted != 0 {
		t.Errorf("stats = %+v", stats)
	}
	if fs.deleteCount(key) != 0 {
		t.Errorf("storage delete count = %d", fs.deleteCount(key))
	}
}

func TestSweeper_PendingIsNotSwept(t *testing.T) {
	// Artefacts still in 'pending' aren't eligible — they may be mid-
	// upload. The sweeper only takes 'ready' (or stale 'deleting').
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()

	key := uuid.NewString()
	seedArtifact(t, pool, key, time.Now().Add(-time.Hour), "pending")

	sw := retention.New(s, fs, silent())
	stats := sw.SweepOnce(context.Background())

	if stats.Claimed != 0 {
		t.Errorf("pending row was claimed: %+v", stats)
	}
}

func TestSweeper_StaleDeletingIsRetried(t *testing.T) {
	// Crash recovery: a row stuck in 'deleting' > grace is re-taken.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()

	key := uuid.NewString()
	seedArtifact(t, pool, key, time.Now().Add(-time.Hour), "deleting")

	// Grace of 5min (default) — the seeded deleted_at is 1 hour old,
	// well past the grace window.
	sw := retention.New(s, fs, silent())
	stats := sw.SweepOnce(context.Background())

	if stats.Claimed != 1 || stats.Deleted != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestSweeper_StorageErrorKeepsRowForRetry(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()

	key := uuid.NewString()
	seedArtifact(t, pool, key, time.Now().Add(-time.Hour), "ready")
	fs.failOn[key] = errors.New("S3 transient")

	sw := retention.New(s, fs, silent())
	stats := sw.SweepOnce(context.Background())

	if stats.StorageFailures != 1 || stats.Deleted != 0 {
		t.Errorf("stats = %+v", stats)
	}
	// Row must still be in the DB (we didn't remove it) but now in
	// 'deleting' state, eligible for retry after the grace window.
	got, err := s.GetArtifactByStorageKey(context.Background(), key)
	if err != nil {
		t.Fatalf("row missing after failed storage delete: %v", err)
	}
	if got.Status != "deleting" {
		t.Errorf("status = %q, want deleting", got.Status)
	}
}

func TestSweeper_PinnedIsNeverSwept(t *testing.T) {
	// Even with expires_at in the past, pinned_at rows stay.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()

	key := uuid.NewString()
	id := seedArtifact(t, pool, key, time.Now().Add(-time.Hour), "ready")
	if _, err := pool.Exec(context.Background(),
		`UPDATE artifacts SET pinned_at = NOW() WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("pin: %v", err)
	}

	sw := retention.New(s, fs, silent())
	stats := sw.SweepOnce(context.Background())

	if stats.Claimed != 0 {
		t.Errorf("pinned row was claimed: %+v", stats)
	}
}

func TestSweeper_StorageNotFoundIsTreatedAsSuccess(t *testing.T) {
	// Someone deleted the object out-of-band; sweeper should still
	// reap the DB row instead of looping forever.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	fs := newFakeStore()

	key := uuid.NewString()
	seedArtifact(t, pool, key, time.Now().Add(-time.Hour), "ready")
	fs.failOn[key] = artifacts.ErrNotFound

	sw := retention.New(s, fs, silent())
	stats := sw.SweepOnce(context.Background())

	if stats.Deleted != 1 {
		t.Errorf("stats = %+v (want deleted=1)", stats)
	}
}
