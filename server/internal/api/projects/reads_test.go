package projects_test

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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func readsHandler(t *testing.T) (*projects.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	return projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))), pool
}

func seedOneProject(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()

	url, branch := "https://github.com/org/demo", "main"
	fp := domain.GitFingerprint(url, branch)
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{{
			Name:   "build",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	return res.ProjectID
}

func TestList_ReturnsProjects(t *testing.T) {
	h, pool := readsHandler(t)
	seedOneProject(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp struct {
		Projects []store.ProjectSummary `json:"projects"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Projects) != 1 || resp.Projects[0].Slug != "demo" {
		t.Fatalf("projects = %+v", resp.Projects)
	}
	if resp.Projects[0].PipelineCount != 1 {
		t.Fatalf("PipelineCount = %d", resp.Projects[0].PipelineCount)
	}
}

func TestList_MethodNotAllowed(t *testing.T) {
	h, _ := readsHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestDetail_NotFound(t *testing.T) {
	h, _ := readsHandler(t)
	// Use chi's mux so URL params get populated.
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}", h.Detail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/nope", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestDetail_ReturnsPipelinesAndRuns(t *testing.T) {
	h, pool := readsHandler(t)
	seedOneProject(t, pool)

	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}", h.Detail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got store.ProjectDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Project.Slug != "demo" {
		t.Fatalf("project: %+v", got.Project)
	}
	if len(got.Pipelines) != 1 {
		t.Fatalf("pipelines = %+v", got.Pipelines)
	}
}

func TestDetail_InvalidRunsParam(t *testing.T) {
	h, pool := readsHandler(t)
	seedOneProject(t, pool)
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}", h.Detail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo?runs=not-a-number", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}
