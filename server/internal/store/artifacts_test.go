package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// artifactParents resolves the pipeline_id + project_id for a given
// run_id so tests can seed artefact rows without hand-crafting the whole
// hierarchy. Uses the shared seedRunningJob from results_test.go.
func artifactParents(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) (pipelineID, projectID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	err := pool.QueryRow(ctx, `
		SELECT p.id, p.project_id
		FROM runs r
		JOIN pipelines p ON p.id = r.pipeline_id
		WHERE r.id = $1
	`, runID).Scan(&pipelineID, &projectID)
	if err != nil {
		t.Fatalf("artifactParents: %v", err)
	}
	return
}

func TestInsertPendingArtifact_Creates(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	pipelineID, projectID := artifactParents(t, pool, runID)

	ttl := time.Now().Add(24 * time.Hour)
	a, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID:      runID,
		JobRunID:   jobID,
		PipelineID: pipelineID,
		ProjectID:  projectID,
		Path:       "bin/core",
		StorageKey: "obj/" + uuid.NewString(),
		ExpiresAt:  &ttl,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if a.Status != "pending" {
		t.Errorf("status = %q, want pending", a.Status)
	}
	if a.Path != "bin/core" {
		t.Errorf("path = %q", a.Path)
	}
	// Pg truncates to microseconds; compare within 1s window.
	if a.ExpiresAt == nil || a.ExpiresAt.Sub(ttl).Abs() > time.Second {
		t.Errorf("expires_at roundtrip: got %v, want ~%v", a.ExpiresAt, ttl)
	}
}

func TestMarkArtifactReady_FlipsStatus(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	pipelineID, projectID := artifactParents(t, pool, runID)
	key := "obj/" + uuid.NewString()

	if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID: runID, JobRunID: jobID, PipelineID: pipelineID, ProjectID: projectID,
		Path: "bin/core", StorageKey: key,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	updated, err := s.MarkArtifactReady(ctx, key, 1024, "deadbeef")
	if err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if !updated {
		t.Fatal("expected update")
	}

	got, err := s.GetArtifactByStorageKey(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "ready" {
		t.Errorf("status = %q", got.Status)
	}
	if got.SizeBytes != 1024 {
		t.Errorf("size = %d", got.SizeBytes)
	}
	if got.ContentSHA256 != "deadbeef" {
		t.Errorf("sha = %q", got.ContentSHA256)
	}

	// Second call must be a no-op (already ready).
	updated2, err := s.MarkArtifactReady(ctx, key, 9999, "cafebabe")
	if err != nil {
		t.Fatalf("mark ready 2: %v", err)
	}
	if updated2 {
		t.Fatal("second mark ready must return false")
	}
	got2, _ := s.GetArtifactByStorageKey(ctx, key)
	if got2.SizeBytes != 1024 || got2.ContentSHA256 != "deadbeef" {
		t.Errorf("second mark must not overwrite: %+v", got2)
	}
}

func TestGetArtifactByStorageKey_NotFound(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	_, err := s.GetArtifactByStorageKey(context.Background(), "never-issued")
	if !errors.Is(err, store.ErrArtifactNotFound) {
		t.Errorf("want ErrArtifactNotFound, got %v", err)
	}
}

func TestListArtifactsByJobRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	pipelineID, projectID := artifactParents(t, pool, runID)

	for i := 0; i < 3; i++ {
		if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
			RunID: runID, JobRunID: jobID, PipelineID: pipelineID, ProjectID: projectID,
			Path: "out/" + uuid.NewString(), StorageKey: "obj/" + uuid.NewString(),
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	got, err := s.ListArtifactsByJobRun(ctx, jobID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d, want 3", len(got))
	}
}

func TestListReadyArtifactsByRun_OnlyReady(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	pipelineID, projectID := artifactParents(t, pool, runID)

	readyKey := "obj/" + uuid.NewString()
	if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID: runID, JobRunID: jobID, PipelineID: pipelineID, ProjectID: projectID,
		Path: "ready/one", StorageKey: readyKey,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkArtifactReady(ctx, readyKey, 10, "abc"); err != nil {
		t.Fatal(err)
	}
	// second row stays pending
	if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID: runID, JobRunID: jobID, PipelineID: pipelineID, ProjectID: projectID,
		Path: "pending/one", StorageKey: "obj/" + uuid.NewString(),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListReadyArtifactsByRun(ctx, runID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].Path != "ready/one" {
		t.Errorf("path = %q", got[0].Path)
	}
}
