package projects_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func checkReportingRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	s := store.New(dbtest.SetupPool(t))
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/check-reporting", h.GetCheckReporting)
	r.Put("/api/v1/projects/{slug}/check-reporting", h.SetCheckReporting)
	return r, s
}

func TestCheckReporting_GetPutValidate(t *testing.T) {
	router, s := checkReportingRouter(t)
	if _, err := s.ApplyProject(t.Context(), store.ApplyProjectInput{Slug: "demo", Name: "demo"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	getMode := func() (string, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo/check-reporting", nil)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("get status = %d", rr.Code)
		}
		var got struct {
			Mode        string `json:"mode"`
			DefaultMode string `json:"default_mode"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got.Mode, got.DefaultMode
	}
	put := func(slug, body string) int {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/"+slug+"/check-reporting",
			strings.NewReader(body))
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr.Code
	}

	// Default is 'both', and the default hint is echoed for the UI.
	if mode, def := getMode(); mode != "both" || def != "both" {
		t.Fatalf("default mode/def = %q/%q, want both/both", mode, def)
	}
	// Round-trip a valid value.
	if code := put("demo", `{"mode":"commit_status"}`); code != http.StatusOK {
		t.Fatalf("put valid = %d, want 200", code)
	}
	if mode, _ := getMode(); mode != "commit_status" {
		t.Errorf("after put, mode = %q, want commit_status", mode)
	}
	// Invalid mode → 400 at the edge (fail-fast; never reaches the DB).
	if code := put("demo", `{"mode":"bogus"}`); code != http.StatusBadRequest {
		t.Errorf("invalid mode status = %d, want 400", code)
	}
	// Unknown project → 404, not an opaque 500.
	if code := put("nope", `{"mode":"both"}`); code != http.StatusNotFound {
		t.Errorf("unknown project status = %d, want 404", code)
	}
}
