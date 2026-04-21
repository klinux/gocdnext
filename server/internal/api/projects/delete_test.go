package projects_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// mountDelete wires the Delete route on a chi router so URLParam
// resolves the {slug} under test. Seeds a project via Apply so
// there's something to delete (and to count pipelines for in the
// happy path). Returns a DELETE-caller keyed by slug.
func mountDelete(t *testing.T) func(string) *httptest.ResponseRecorder {
	t.Helper()
	h, _ := newHandler(t)
	r := chi.NewRouter()
	r.Delete("/api/v1/projects/{slug}", h.Delete)

	body := map[string]any{
		"slug": "to-delete",
		"name": "To Delete",
		"files": []map[string]string{
			{"name": "build.yaml", "content": sampleFile},
		},
	}
	if rr := doApply(t, h, body); rr.Code != http.StatusOK {
		t.Fatalf("seed apply: %d %s", rr.Code, rr.Body.String())
	}

	return func(slug string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/"+slug, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}
}

func TestDelete_RemovesProjectAndReturnsCounts(t *testing.T) {
	do := mountDelete(t)

	rr := do("to-delete")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	// The seeded project has 1 pipeline; runs/secrets/scm_sources = 0.
	body := rr.Body.String()
	if !strings.Contains(body, `"pipelines_deleted":1`) {
		t.Fatalf("expected pipelines_deleted:1 in %s", body)
	}
	if !strings.Contains(body, `"slug":"to-delete"`) {
		t.Fatalf("expected slug echo in %s", body)
	}

	// Second delete → 404 because the row is gone.
	rr2 := do("to-delete")
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", rr2.Code)
	}
}

func TestDelete_UnknownSlug404(t *testing.T) {
	do := mountDelete(t)
	rr := do("does-not-exist")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestDelete_MethodNotAllowed(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/x", nil)
	rr := httptest.NewRecorder()
	h.Delete(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rr.Code)
	}
}

