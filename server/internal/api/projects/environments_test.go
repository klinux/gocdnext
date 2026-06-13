package projects_test

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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func environmentsRouter(t *testing.T) (http.Handler, *store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/environments", h.ListEnvironments)
	r.Get("/api/v1/projects/{slug}/environments/{envID}/deployments", h.ListEnvironmentDeployments)
	return r, s, pool
}

// mustMarkSuccess finalizes a revision to success directly — the
// dispatch/result wiring is exercised in the scheduler/store tests;
// here we only need a finalized row for the read path.
func mustMarkSuccess(t *testing.T, pool *pgxpool.Pool, revID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE deployment_revisions SET status='success', finished_at=NOW() WHERE id=$1`, revID)
	if err != nil {
		t.Fatalf("mark success: %v", err)
	}
}

func seedProjectForEnv(t *testing.T, s *store.Store, slug string) uuid.UUID {
	t.Helper()
	if _, err := s.ApplyProject(t.Context(), store.ApplyProjectInput{Slug: slug, Name: slug}); err != nil {
		t.Fatalf("seed project %s: %v", slug, err)
	}
	d, err := s.GetProjectDetail(t.Context(), slug, 1)
	if err != nil {
		t.Fatalf("project detail %s: %v", slug, err)
	}
	return d.Project.ID
}

func TestListEnvironments_ShapeAndCurrent(t *testing.T) {
	router, s, pool := environmentsRouter(t)
	ctx := t.Context()
	projectID := seedProjectForEnv(t, s, "demo")

	// staging: no deploy → current must be explicit null.
	if _, err := s.EnsureEnvironment(ctx, projectID, "staging"); err != nil {
		t.Fatalf("ensure staging: %v", err)
	}
	// production: a successful deploy → current populated.
	prod, _ := s.EnsureEnvironment(ctx, projectID, "production")
	rev, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: prod, Attempt: 0, Version: "1.42.abc", DeployedBy: "alice",
	})
	if err != nil {
		t.Fatalf("create revision: %v", err)
	}
	mustMarkSuccess(t, pool, rev)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo/environments", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	// Contract: an undeployed env emits "current":null, never an
	// absent field (the TS type is DeploymentRecord | null).
	if !strings.Contains(body, `"current":null`) {
		t.Fatalf("expected an explicit \"current\":null in body, got: %s", body)
	}

	var resp struct {
		Environments []struct {
			Name    string `json:"name"`
			Current *struct {
				Version string `json:"version"`
				Status  string `json:"status"`
			} `json:"current"`
		} `json:"environments"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Environments) != 2 {
		t.Fatalf("got %d environments, want 2", len(resp.Environments))
	}
	byName := map[string]*struct {
		Version string `json:"version"`
		Status  string `json:"status"`
	}{}
	for _, e := range resp.Environments {
		byName[e.Name] = e.Current
	}
	if byName["staging"] != nil {
		t.Errorf("staging.current = %+v, want null", byName["staging"])
	}
	if c := byName["production"]; c == nil || c.Version != "1.42.abc" || c.Status != "success" {
		t.Errorf("production.current = %+v, want version 1.42.abc / success", c)
	}
}

func TestListEnvironmentDeployments_CrossProject404(t *testing.T) {
	router, s, _ := environmentsRouter(t)
	ctx := t.Context()
	seedProjectForEnv(t, s, "demo")
	otherID := seedProjectForEnv(t, s, "other")

	// An environment that belongs to "other", read through "demo".
	otherEnv, _ := s.EnsureEnvironment(ctx, otherID, "production")

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/demo/environments/"+otherEnv.String()+"/deployments", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-project read status = %d, want 404 (scope guard)", rr.Code)
	}
}

func TestListEnvironments_UnknownProject404(t *testing.T) {
	router, _, _ := environmentsRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/nope/environments", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}
