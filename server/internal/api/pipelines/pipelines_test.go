package pipelines_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/pipelines"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newHandler(t *testing.T) (*pipelines.Handler, *store.Store) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	return pipelines.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))), s
}

func seedPipeline(t *testing.T, s *store.Store) uuid.UUID {
	t.Helper()
	applied, err := s.ApplyProject(context.Background(), store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{{
			Name:   "build",
			Stages: []string{"build", "test"},
			Materials: []domain.Material{{
				Type: domain.MaterialManual, Fingerprint: domain.ManualFingerprint(),
			}},
			Jobs: []domain.Job{
				{Name: "vet", Stage: "build", Image: "golang:1.25",
					Tasks: []domain.Task{{Script: "go vet ./..."}}},
				{Name: "unit", Stage: "test", Image: "golang:1.25", Docker: true,
					Needs: []string{"vet"},
					Tasks: []domain.Task{{Script: "go test -race ./..."}}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return applied.Pipelines[0].PipelineID
}

func TestYAML_RendersStoredDefinition(t *testing.T) {
	h, s := newHandler(t)
	id := seedPipeline(t, s)

	r := chi.NewRouter()
	r.Get("/api/v1/pipelines/{id}/yaml", h.YAML)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pipelines/"+id.String()+"/yaml", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		YAML          string `json:"yaml"`
		Reconstructed bool   `json:"reconstructed"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Reconstructed {
		t.Fatalf("expected reconstructed=true")
	}
	// Spot-check fields the stale v1 reconstruction was silently
	// dropping: image, script line, docker flag, needs.
	for _, want := range []string{
		"name: build",
		"stages: [build, test]",
		"image: golang:1.25",
		"go test -race ./...",
		"docker: true",
		"needs: [vet]",
	} {
		if !strings.Contains(body.YAML, want) {
			t.Errorf("YAML missing %q, got:\n%s", want, body.YAML)
		}
	}
}

func TestYAML_NotFound(t *testing.T) {
	h, _ := newHandler(t)
	r := chi.NewRouter()
	r.Get("/api/v1/pipelines/{id}/yaml", h.YAML)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pipelines/"+uuid.NewString()+"/yaml", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestYAML_BadID(t *testing.T) {
	h, _ := newHandler(t)
	r := chi.NewRouter()
	r.Get("/api/v1/pipelines/{id}/yaml", h.YAML)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pipelines/not-a-uuid/yaml", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}
