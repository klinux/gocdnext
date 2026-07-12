package projects_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/deploysvc"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// fakeValidator stands in for the provider's Application fetch so the API tests
// don't need a live cluster; the error it returns drives the Fault→HTTP mapping.
type fakeValidator struct{ err error }

func (f fakeValidator) ValidateSingleSource(context.Context, deploy.DeploymentTarget) error {
	return f.err
}

func newDeployTargetsRouter(t *testing.T, validatorErr error, withRegistrar bool) (http.Handler, *store.Store) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if withRegistrar {
		h = h.WithDeployRegistrar(deploysvc.New(fakeValidator{err: validatorErr}, s))
	}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/deploy-targets", h.ListDeployTargets)
	r.Post("/api/v1/projects/{slug}/deploy-targets", h.SetDeployTarget)
	r.Delete("/api/v1/projects/{slug}/deploy-targets/{env}", h.DeleteDeployTarget)
	return r, s
}

func seedProjectAndCluster(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "demo"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := s.InsertCluster(ctx, nil, store.ClusterInput{Name: "prod", AuthType: store.ClusterAuthInCluster}); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
}

func doReq(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func TestDeployTargets_RegisterListDelete(t *testing.T) {
	r, s := newDeployTargetsRouter(t, nil, true) // validator passes (single-source)
	seedProjectAndCluster(t, s)

	// Register (no namespace → defaults to argocd).
	rr := doReq(r, http.MethodPost, "/api/v1/projects/demo/deploy-targets",
		`{"environment":"production","cluster":"prod","application":"checkout","sync_mode":"trigger"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("register status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"namespace":"argocd"`) || !strings.Contains(rr.Body.String(), `"application":"checkout"`) {
		t.Errorf("register body missing defaulted/canonical fields: %s", rr.Body.String())
	}

	// List shows it.
	rr = doReq(r, http.MethodGet, "/api/v1/projects/demo/deploy-targets", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"environment":"production"`) {
		t.Fatalf("list status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Delete → 204, then 404 (idempotent-not-found).
	if rr := doReq(r, http.MethodDelete, "/api/v1/projects/demo/deploy-targets/production", ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodDelete, "/api/v1/projects/demo/deploy-targets/production", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", rr.Code)
	}
}

func TestSetDeployTarget_FaultMapping(t *testing.T) {
	t.Run("multi-source -> 422", func(t *testing.T) {
		r, s := newDeployTargetsRouter(t, fmt.Errorf("app: %w", deploy.ErrMultiSource), true)
		seedProjectAndCluster(t, s)
		rr := doReq(r, http.MethodPost, "/api/v1/projects/demo/deploy-targets",
			`{"environment":"production","cluster":"prod","application":"checkout","sync_mode":"trigger"}`)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422, body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("bad sync_mode -> 400 (before the fetch)", func(t *testing.T) {
		r, s := newDeployTargetsRouter(t, nil, true)
		seedProjectAndCluster(t, s)
		rr := doReq(r, http.MethodPost, "/api/v1/projects/demo/deploy-targets",
			`{"environment":"production","cluster":"prod","application":"checkout","sync_mode":"auto"}`)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("unknown project -> 404", func(t *testing.T) {
		r, _ := newDeployTargetsRouter(t, nil, true)
		rr := doReq(r, http.MethodGet, "/api/v1/projects/nope/deploy-targets", "")
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
	})
}

func TestSetDeployTarget_NotConfigured_501(t *testing.T) {
	r, s := newDeployTargetsRouter(t, nil, false) // no registrar wired
	seedProjectAndCluster(t, s)
	rr := doReq(r, http.MethodPost, "/api/v1/projects/demo/deploy-targets",
		`{"environment":"production","cluster":"prod","application":"checkout","sync_mode":"trigger"}`)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
}
