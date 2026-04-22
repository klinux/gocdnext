package projects_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/configsync"
	gh "github.com/gocdnext/gocdnext/server/internal/scm/github"
)

// doSync POSTs /api/v1/projects/{slug}/sync via a chi router so
// the URL-param match lines up with the handler's chi.URLParam
// call — httptest.NewRequest alone doesn't seed the context.
func doSync(t *testing.T, h *projects.Handler, slug string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{slug}/sync", h.Sync)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+slug+"/sync", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// seedProjectWithSCM applies a project bound to an scm_source (no
// pipeline files yet) via the normal Apply path, so Sync has a
// real row to operate on. Returns the slug for convenience.
func seedProjectWithSCM(t *testing.T, h *projects.Handler, slug string) {
	t.Helper()
	rr := doApply(t, h, map[string]any{
		"slug":  slug,
		"name":  slug,
		"files": []map[string]string{},
		"scm_source": map[string]string{
			"provider":       "github",
			"url":            "https://github.com/org/" + slug,
			"default_branch": "main",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("seed apply: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSync_AppliesRemoteFiles(t *testing.T) {
	h, fetcher := newHandlerWithFetcher(t)
	// Bind the project with zero pipelines (fetcher returns nothing).
	seedProjectWithSCM(t, h, "remote")
	fetcher.calls = 0 // reset — only count the Sync call

	// Stage a real pipeline for the sync call.
	fetcher.files = []gh.RawFile{{Name: "pipeline.yaml", Content: samplePipelineYAML}}

	rr := doSync(t, h, "remote")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetcher calls = %d, want 1", fetcher.calls)
	}
	if fetcher.last.ref != "main" {
		t.Fatalf("fetcher ref = %q, want main", fetcher.last.ref)
	}

	var resp projects.ApplyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Pipelines) != 1 || resp.Pipelines[0].Name != "built-from-remote" {
		t.Fatalf("pipelines = %+v", resp.Pipelines)
	}
	if !resp.Pipelines[0].Created {
		t.Fatalf("pipeline should be newly created")
	}
}

func TestSync_RemovesPipelinesDroppedFromRepo(t *testing.T) {
	h, fetcher := newHandlerWithFetcher(t)
	seedProjectWithSCM(t, h, "shrink")
	fetcher.files = []gh.RawFile{{Name: "pipeline.yaml", Content: samplePipelineYAML}}
	if rr := doSync(t, h, "shrink"); rr.Code != http.StatusOK {
		t.Fatalf("first sync: %d %s", rr.Code, rr.Body.String())
	}

	// Now the repo no longer contains the pipeline — sync must
	// remove it (mirrors Apply's "wanted minus existing" diff).
	fetcher.files = nil
	rr := doSync(t, h, "shrink")
	if rr.Code != http.StatusOK {
		t.Fatalf("second sync: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp projects.ApplyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.PipelinesRemoved) != 1 || resp.PipelinesRemoved[0] != "built-from-remote" {
		t.Fatalf("expected pipeline removed, got %+v", resp)
	}
	// Empty-folder warning should surface so the UI can flag it.
	if len(resp.Warnings) == 0 {
		t.Fatalf("expected at least one warning, got none")
	}
}

func TestSync_FolderMissingWarnsButSucceeds(t *testing.T) {
	h, fetcher := newHandlerWithFetcher(t)
	seedProjectWithSCM(t, h, "no-folder")
	fetcher.err = fmt.Errorf("wrap: %w", configsync.ErrFolderNotFound)

	rr := doSync(t, h, "no-folder")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp projects.ApplyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Warnings) == 0 {
		t.Fatalf("expected folder-not-found warning, got none")
	}
}

func TestSync_RequiresSCMSource(t *testing.T) {
	h, _ := newHandlerWithFetcher(t)
	// Apply a project with NO scm_source — Sync has nothing to
	// pull from and must refuse.
	rr := doApply(t, h, map[string]any{
		"slug":  "bare",
		"name":  "Bare",
		"files": []map[string]string{{"name": "build.yaml", "content": sampleFile}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}
	rr = doSync(t, h, "bare")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSync_UnknownProject(t *testing.T) {
	h, _ := newHandlerWithFetcher(t)
	rr := doSync(t, h, "does-not-exist")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestSync_NoFetcherWired(t *testing.T) {
	h, _ := newHandler(t) // without WithConfigFetcher
	// Seed must use the CLI-style Apply path (no fetcher) so the
	// project exists for the Sync call to reach its 503 check.
	rr := doApply(t, h, map[string]any{
		"slug":  "no-fetcher",
		"name":  "No Fetcher",
		"files": []map[string]string{{"name": "build.yaml", "content": sampleFile}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}
	rr = doSync(t, h, "no-fetcher")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}
