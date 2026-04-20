package runs_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/runs"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func testArtifactStore(t *testing.T) *artifacts.FilesystemStore {
	t.Helper()
	signer, err := artifacts.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	fs, err := artifacts.NewFilesystemStore(t.TempDir(), "http://unit-test", signer)
	if err != nil {
		t.Fatalf("fs: %v", err)
	}
	return fs
}

func TestArtifacts_NotConfigured_503(t *testing.T) {
	// Base handler without WithArtifactStore returns 503 — makes it
	// obvious to the UI that the backend disabled artefacts.
	pool := dbtest.SetupPool(t)
	h := runs.NewHandler(store.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))

	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}/artifacts", h.Artifacts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+uuid.NewString()+"/artifacts", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestArtifacts_InvalidID_400(t *testing.T) {
	pool := dbtest.SetupPool(t)
	h := runs.NewHandler(store.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithArtifactStore(testArtifactStore(t))

	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}/artifacts", h.Artifacts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/not-a-uuid/artifacts", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestArtifacts_UnknownRun_ReturnsEmpty(t *testing.T) {
	pool := dbtest.SetupPool(t)
	h := runs.NewHandler(store.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithArtifactStore(testArtifactStore(t))

	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}/artifacts", h.Artifacts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+uuid.NewString()+"/artifacts", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got []runs.ArtifactResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %d rows", len(got))
	}
}

func TestArtifacts_ListsPendingAndReady_WithDownloadOnlyForReady(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	runID := seedRun(t, pool)
	h := runs.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithArtifactStore(testArtifactStore(t))

	// Resolve the seeded job_run + pipeline + project so we can insert artifacts.
	ctx := context.Background()
	var jobID, pipelineID, projectID uuid.UUID
	err := pool.QueryRow(ctx, `
		SELECT jr.id, r.pipeline_id, p.project_id
		FROM job_runs jr
		JOIN runs r ON r.id = jr.run_id
		JOIN pipelines p ON p.id = r.pipeline_id
		WHERE jr.run_id = $1
		LIMIT 1
	`, runID).Scan(&jobID, &pipelineID, &projectID)
	if err != nil {
		t.Fatalf("resolve parents: %v", err)
	}

	readyKey := "obj/ready-" + uuid.NewString()
	if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID: runID, JobRunID: jobID, PipelineID: pipelineID, ProjectID: projectID,
		Path: "bin/core", StorageKey: readyKey,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkArtifactReady(ctx, readyKey, 1024, "deadbeef"); err != nil {
		t.Fatal(err)
	}

	pendingKey := "obj/pending-" + uuid.NewString()
	if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID: runID, JobRunID: jobID, PipelineID: pipelineID, ProjectID: projectID,
		Path: "coverage.out", StorageKey: pendingKey,
	}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}/artifacts", h.Artifacts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID.String()+"/artifacts", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got []runs.ArtifactResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}

	byPath := map[string]runs.ArtifactResponse{}
	for _, a := range got {
		byPath[a.Path] = a
	}
	ready, ok := byPath["bin/core"]
	if !ok {
		t.Fatal("missing ready row")
	}
	if ready.Status != "ready" || ready.SizeBytes != 1024 || ready.ContentSHA256 != "deadbeef" {
		t.Errorf("ready row: %+v", ready)
	}
	if ready.DownloadURL == "" {
		t.Error("ready row should have download URL")
	}
	if ready.JobName == "" {
		t.Error("ready row missing job_name")
	}

	pending, ok := byPath["coverage.out"]
	if !ok {
		t.Fatal("missing pending row")
	}
	if pending.Status != "pending" {
		t.Errorf("pending row status = %q", pending.Status)
	}
	if pending.DownloadURL != "" {
		t.Error("pending row must not have download URL")
	}
}
