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

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
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
	r.Post("/api/v1/projects/{slug}/environments/{envID}/rollback", h.RollbackEnvironment)
	r.Delete("/api/v1/projects/{slug}/environments/{envID}", h.DeleteEnvironment)
	return r, s, pool
}

// deleteEnvReq issues DELETE /environments/{envID}; a nil user models auth
// disabled (treated as admin), otherwise the user rides the request context.
func deleteEnvReq(t *testing.T, router http.Handler, slug, envID string, user *store.User) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/projects/"+slug+"/environments/"+envID, nil)
	if user != nil {
		req = req.WithContext(authapi.WithUser(req.Context(), *user))
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr.Code
}

func postRollback(t *testing.T, router http.Handler, slug, envID, toRevision string) int {
	t.Helper()
	body := strings.NewReader(`{"to_revision_id":"` + toRevision + `"}`)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+slug+"/environments/"+envID+"/rollback", body)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr.Code
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

func TestDeleteEnvironment_Handler(t *testing.T) {
	router, s, _ := environmentsRouter(t)
	ctx := t.Context()
	projectID := seedProjectForEnv(t, s, "envdel")

	// Malformed id → 400 before any authz/store work.
	if code := deleteEnvReq(t, router, "envdel", "not-a-uuid", nil); code != http.StatusBadRequest {
		t.Fatalf("malformed id: got %d, want 400", code)
	}

	envID, err := s.EnsureEnvironment(ctx, projectID, "prod")
	if err != nil {
		t.Fatalf("EnsureEnvironment: %v", err)
	}

	// A maintainer is refused (403) and the env survives — the cascade is admin-only.
	maintainer := store.User{ID: uuid.New(), Role: store.RoleMaintainer}
	if code := deleteEnvReq(t, router, "envdel", envID.String(), &maintainer); code != http.StatusForbidden {
		t.Fatalf("maintainer delete: got %d, want 403", code)
	}
	if envs, _ := s.ListEnvironments(ctx, projectID); len(envs) != 1 {
		t.Fatalf("403 path removed the env: %+v", envs)
	}

	admin := store.User{ID: uuid.New(), Role: store.RoleAdmin}

	// An admin naming an absent env → 404.
	if code := deleteEnvReq(t, router, "envdel", uuid.New().String(), &admin); code != http.StatusNotFound {
		t.Fatalf("absent env: got %d, want 404", code)
	}

	// The admin deletes the real env → 204, and it's gone.
	if code := deleteEnvReq(t, router, "envdel", envID.String(), &admin); code != http.StatusNoContent {
		t.Fatalf("admin delete: got %d, want 204", code)
	}
	if envs, _ := s.ListEnvironments(ctx, projectID); len(envs) != 0 {
		t.Fatalf("env survived admin delete: %+v", envs)
	}

	// Auth disabled (no user in context) is treated as admin.
	envID2, err := s.EnsureEnvironment(ctx, projectID, "prod")
	if err != nil {
		t.Fatalf("EnsureEnvironment (re-create): %v", err)
	}
	if code := deleteEnvReq(t, router, "envdel", envID2.String(), nil); code != http.StatusNoContent {
		t.Fatalf("auth-disabled delete: got %d, want 204", code)
	}

	// An env with an in-flight deploy (an in_progress revision) → 409 even for an
	// admin: deleting it would cascade the revision out from under the running job.
	active, err := s.EnsureEnvironment(ctx, projectID, "prod-active")
	if err != nil {
		t.Fatalf("EnsureEnvironment (active): %v", err)
	}
	if _, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: active, Attempt: 0, Version: "9.9.inflight",
	}); err != nil {
		t.Fatalf("CreateDeploymentRevision: %v", err)
	}
	if code := deleteEnvReq(t, router, "envdel", active.String(), &admin); code != http.StatusConflict {
		t.Fatalf("active-deploy delete: got %d, want 409", code)
	}
	if envs, _ := s.ListEnvironments(ctx, projectID); len(envs) != 1 {
		t.Fatalf("409 path removed the active env: %+v", envs)
	}
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

func TestRollbackEnvironment_ErrorMapping(t *testing.T) {
	router, s, pool := environmentsRouter(t)
	ctx := t.Context()
	projectID := seedProjectForEnv(t, s, "demo")
	envID, _ := s.EnsureEnvironment(ctx, projectID, "production")

	// in_progress revision → 422 (not a successful deploy).
	inProgress, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, Version: "wip",
	})
	// success revision but run garbage-collected (job_run NULL) → 422.
	runGone, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, Version: "orphan",
	})
	if _, err := pool.Exec(ctx,
		`UPDATE deployment_revisions SET status='success', finished_at=NOW() WHERE id=$1`, runGone); err != nil {
		t.Fatalf("mark success: %v", err)
	}

	tests := []struct {
		name  string
		envID string
		rev   string
		want  int
	}{
		{"not a deploy success", envID.String(), inProgress.String(), http.StatusUnprocessableEntity},
		{"run garbage-collected", envID.String(), runGone.String(), http.StatusUnprocessableEntity},
		{"unknown revision", envID.String(), uuid.NewString(), http.StatusNotFound},
		{"malformed revision id", envID.String(), "not-a-uuid", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := postRollback(t, router, "demo", tt.envID, tt.rev); got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
		})
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
