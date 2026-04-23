package projects_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

type cachesTestEnv struct {
	router    http.Handler
	store     *store.Store
	projectID uuid.UUID
	// fsRoot is the directory the FilesystemStore writes blobs
	// into. Exposed so the purge test can seed a marker file at
	// exactly the path the handler is expected to delete.
	fsRoot string
}

// cachesRouter wires the caches endpoints against a real
// filesystem artifact store so the purge test exercises the
// actual blob delete path. A fake-only test would miss a
// regression where the handler stops calling Delete.
func cachesRouter(t *testing.T, withArtifacts bool) cachesTestEnv {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var root string
	if withArtifacts {
		root = t.TempDir()
		signer, err := artifacts.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
		if err != nil {
			t.Fatalf("signer: %v", err)
		}
		fs, err := artifacts.NewFilesystemStore(root, "http://unit", signer)
		if err != nil {
			t.Fatalf("fs store: %v", err)
		}
		h = h.WithArtifactStore(fs)
	}

	if _, err := s.ApplyProject(t.Context(), store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	detail, err := s.GetProjectDetail(t.Context(), "demo", 1)
	if err != nil {
		t.Fatalf("project detail: %v", err)
	}

	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/caches", h.ListCaches)
	r.Delete("/api/v1/projects/{slug}/caches/{id}", h.PurgeCache)
	return cachesTestEnv{
		router:    r,
		store:     s,
		projectID: detail.Project.ID,
		fsRoot:    root,
	}
}

func TestListCaches_IncludesPendingAndReadyWithTotals(t *testing.T) {
	env := cachesRouter(t, false)
	ctx := t.Context()

	ready, _ := env.store.UpsertPendingCache(ctx, env.projectID, "ready-key")
	_ = env.store.MarkCacheReady(ctx, ready.ID, 1024, "abc")
	_, _ = env.store.UpsertPendingCache(ctx, env.projectID, "pending-key")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo/caches", nil)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Caches []struct {
			ID     string `json:"id"`
			Key    string `json:"key"`
			Status string `json:"status"`
			Size   int64  `json:"size_bytes"`
		} `json:"caches"`
		TotalBytes int64 `json:"total_bytes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if len(resp.Caches) != 2 {
		t.Fatalf("expected 2 rows, got %d (%+v)", len(resp.Caches), resp.Caches)
	}
	// total_bytes counts only ready rows so the UI's "footprint"
	// number matches what the eviction sweeper sees on disk.
	if resp.TotalBytes != 1024 {
		t.Errorf("total_bytes = %d, want 1024 (pending excluded)", resp.TotalBytes)
	}
	// Pending MUST be listed so a stuck upload is visible — not
	// hidden behind the "ready" semantic that agents use.
	var sawPending bool
	for _, c := range resp.Caches {
		if c.Status == "pending" {
			sawPending = true
		}
	}
	if !sawPending {
		t.Error("pending cache missing from list response")
	}
}

func TestListCaches_UnknownProject404(t *testing.T) {
	env := cachesRouter(t, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/nope/caches", nil)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestPurgeCache_DeletesBlobAndRow(t *testing.T) {
	env := cachesRouter(t, true)
	ctx := t.Context()

	c, _ := env.store.UpsertPendingCache(ctx, env.projectID, "k")
	_ = env.store.MarkCacheReady(ctx, c.ID, 4096, "sha")

	// Seed the blob on disk so the Delete call is observable.
	// FilesystemStore.Delete returns ErrNotFound if there's no
	// file (the handler treats that as success), so materialising
	// the file first proves we're on the success branch.
	blobPath := filepath.Join(env.fsRoot, c.StorageKey)
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatalf("mkdir blob dir: %v", err)
	}
	if err := os.WriteFile(blobPath, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/demo/caches/"+c.ID.String(), nil)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
		t.Errorf("blob still on disk after purge: %v", err)
	}
	if _, err := env.store.GetReadyCacheByKey(ctx, env.projectID, "k"); err == nil {
		t.Error("row still present after purge")
	}
}

func TestPurgeCache_UnknownIDReturns404(t *testing.T) {
	env := cachesRouter(t, true)
	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/projects/demo/caches/11111111-1111-1111-1111-111111111111", nil)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestPurgeCache_MalformedIDReturns400(t *testing.T) {
	env := cachesRouter(t, true)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/demo/caches/not-a-uuid", nil)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestPurgeCache_NoArtifactBackendReturns503(t *testing.T) {
	// Handler without WithArtifactStore must refuse purge — the
	// alternative (delete the row but leak the blob) is worse
	// than making the operator fix the config.
	env := cachesRouter(t, false)
	c, _ := env.store.UpsertPendingCache(t.Context(), env.projectID, "k")
	_ = env.store.MarkCacheReady(t.Context(), c.ID, 1, "x")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/demo/caches/"+c.ID.String(), nil)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}
