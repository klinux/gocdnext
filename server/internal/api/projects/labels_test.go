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

func labelsRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	s := store.New(dbtest.SetupPool(t))
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/labels", h.ListLabels)
	r.Put("/api/v1/projects/{slug}/labels", h.SetLabels)
	return r, s
}

func putLabels(t *testing.T, router http.Handler, slug, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/"+slug+"/labels",
		strings.NewReader(body))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr.Code
}

func TestLabels_SetGetTrimAndValidate(t *testing.T) {
	router, s := labelsRouter(t)
	if _, err := s.ApplyProject(t.Context(), store.ApplyProjectInput{Slug: "demo", Name: "demo"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Set with surrounding whitespace → trimmed server-side.
	if code := putLabels(t, router, "demo",
		`{"labels":[{"key":"  team ","value":" payments "},{"key":"tier","value":"critical"}]}`); code != http.StatusNoContent {
		t.Fatalf("put status = %d", code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo/labels", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status = %d", rr.Code)
	}
	var got struct {
		Labels []store.ProjectLabel `json:"labels"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Ordered key,value → team before tier; trimmed.
	if len(got.Labels) != 2 || got.Labels[0] != (store.ProjectLabel{Key: "team", Value: "payments"}) {
		t.Fatalf("labels = %+v", got.Labels)
	}

	// key required → 400.
	if code := putLabels(t, router, "demo", `{"labels":[{"key":"","value":"x"}]}`); code != http.StatusBadRequest {
		t.Fatalf("empty key status = %d, want 400", code)
	}

	// too long → 400.
	long := strings.Repeat("a", 101)
	if code := putLabels(t, router, "demo", `{"labels":[{"key":"`+long+`","value":"x"}]}`); code != http.StatusBadRequest {
		t.Fatalf("too-long status = %d, want 400", code)
	}

	// ':' is the wire/display separator, so keys cannot contain it.
	if code := putLabels(t, router, "demo", `{"labels":[{"key":"team:backend","value":"x"}]}`); code != http.StatusBadRequest {
		t.Fatalf("colon key status = %d, want 400", code)
	}

	// unknown project → 404.
	if code := putLabels(t, router, "nope", `{"labels":[]}`); code != http.StatusNotFound {
		t.Fatalf("unknown project status = %d, want 404", code)
	}
}

func TestLabels_TooMany(t *testing.T) {
	router, s := labelsRouter(t)
	if _, err := s.ApplyProject(t.Context(), store.ApplyProjectInput{Slug: "demo", Name: "demo"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var b strings.Builder
	b.WriteString(`{"labels":[`)
	for i := 0; i < 51; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"key":"k`)
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(`","value":"v"}`)
	}
	b.WriteString(`]}`)
	if code := putLabels(t, router, "demo", b.String()); code != http.StatusBadRequest {
		t.Fatalf("too-many status = %d, want 400", code)
	}
}
