package projects_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedInflightWatch sets up project "demo" + cluster + env + an in_progress revision +
// a deploy_watch, and returns the store.
func seedInflightWatch(t *testing.T) *store.Store {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	seedProjectAndCluster(t, s) // project "demo" + cluster "prod"

	detail, err := s.GetProjectDetail(ctx, "demo", 1)
	if err != nil {
		t.Fatalf("project detail: %v", err)
	}
	projectID := detail.Project.ID
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, Attempt: 0, Version: "1.4.2", DeployedBy: "svc",
	})
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	if _, err := s.CreateDeployWatch(ctx, store.DeployWatchInput{
		DeploymentRevisionID: revID, ProjectID: projectID, SyncMode: "trigger",
		Cluster: "prod", Application: "checkout", Namespace: "argocd",
		ExpectedRevision: "abc0123456789", DeadlineAt: time.Now().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	return s
}

func deployWatchesReq(r http.Handler, role string) map[string]any {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo/deploy-watches", nil)
	if role != "" {
		req = req.WithContext(authapi.WithUser(req.Context(), store.User{ID: uuid.New(), Role: role}))
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return body
}

func TestListDeployWatches_RoleSanitized(t *testing.T) {
	s := seedInflightWatch(t)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/deploy-watches", h.ListDeployWatches)

	// Maintainer sees the config (application/cluster/sync_mode).
	m := deployWatchesReq(r, store.RoleMaintainer)
	watches, _ := m["deploy_watches"].([]any)
	if len(watches) != 1 {
		t.Fatalf("maintainer watches = %v, want 1", watches)
	}
	mw, _ := watches[0].(map[string]any)
	if mw["environment"] != "production" || mw["version"] != "1.4.2" {
		t.Fatalf("maintainer view missing live fields: %v", mw)
	}
	if mw["application"] != "checkout" || mw["cluster"] != "prod" || mw["sync_mode"] != "trigger" {
		t.Fatalf("maintainer must see config, got: %v", mw)
	}

	// Viewer sees live state but NOT the config.
	v := deployWatchesReq(r, store.RoleViewer)
	vwatches, _ := v["deploy_watches"].([]any)
	vw, _ := vwatches[0].(map[string]any)
	if vw["environment"] != "production" || vw["version"] != "1.4.2" || vw["expected_revision"] != "abc0123456789" {
		t.Fatalf("viewer must see live state: %v", vw)
	}
	for _, k := range []string{"application", "cluster", "sync_mode"} {
		if _, present := vw[k]; present {
			t.Errorf("viewer leaked maintainer-only config field %q: %v", k, vw)
		}
	}
}
