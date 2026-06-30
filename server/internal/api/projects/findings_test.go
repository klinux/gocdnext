package projects_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func findingsRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	s := store.New(dbtest.SetupPool(t))
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/findings", h.ListFindings)
	return r, s
}

func getFindings(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

func TestFindings_ValidationAndShape(t *testing.T) {
	router, s := findingsRouter(t)
	if _, err := s.ApplyProject(t.Context(), store.ApplyProjectInput{Slug: "demo", Name: "demo"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Invalid severity → 400.
	if rr := getFindings(t, router, "/api/v1/projects/demo/findings?severity=bogus"); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad severity status = %d, want 400", rr.Code)
	}
	// Invalid limit → 400.
	if rr := getFindings(t, router, "/api/v1/projects/demo/findings?limit=0"); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad limit status = %d, want 400", rr.Code)
	}
	// Unknown project → 404.
	if rr := getFindings(t, router, "/api/v1/projects/nope/findings"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown project status = %d, want 404", rr.Code)
	}

	// Happy path (no findings) → 200 with empty list + default limit echoed.
	rr := getFindings(t, router, "/api/v1/projects/demo/findings?limit=999")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Findings       []store.Finding  `json:"findings"`
		Total          int64            `json:"total"`
		SeverityCounts map[string]int64 `json:"severity_counts"`
		Limit          int32            `json:"limit"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 0 || len(body.Findings) != 0 {
		t.Fatalf("expected empty findings, got %+v", body)
	}
	// limit=999 clamped to the 200 max.
	if body.Limit != 200 {
		t.Fatalf("limit = %d, want clamped to 200", body.Limit)
	}
}
