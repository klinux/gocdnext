package projects_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newPRHeadTrustRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/pr-head-config", h.GetPRHeadTrust)
	r.Put("/api/v1/projects/{slug}/pr-head-config", h.SetPRHeadTrust)
	return r, s
}

// The mutation is admin-only (enabling it hands same-repo PR authors control of
// the executable graph). A maintainer must be refused and the flag must stay
// off; only an admin can flip it, and the read reflects the flip.
func TestPRHeadTrust_AdminOnlyPut(t *testing.T) {
	r, s := newPRHeadTrustRouter(t)
	if _, err := s.ApplyProject(context.Background(), store.ApplyProjectInput{Slug: "demo", Name: "demo"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	const path = "/api/v1/projects/demo/pr-head-config"

	// Maintainer cannot enable it.
	if rr := doReqAs(r, http.MethodPut, path, `{"enabled":true}`, store.RoleMaintainer); rr.Code != http.StatusForbidden {
		t.Fatalf("maintainer PUT = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	// ...and it stayed disabled (a maintainer can still read it).
	if rr := doReqAs(r, http.MethodGet, path, "", store.RoleMaintainer); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"enabled":false`) {
		t.Fatalf("GET after blocked PUT = %d %s, want 200 enabled:false", rr.Code, rr.Body.String())
	}

	// Admin enables it.
	if rr := doReqAs(r, http.MethodPut, path, `{"enabled":true}`, store.RoleAdmin); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"enabled":true`) {
		t.Fatalf("admin PUT = %d %s, want 200 enabled:true", rr.Code, rr.Body.String())
	}
	// The read now reflects the enabled state.
	if rr := doReqAs(r, http.MethodGet, path, "", store.RoleMaintainer); !strings.Contains(rr.Body.String(), `"enabled":true`) {
		t.Fatalf("GET after enable = %s, want enabled:true", rr.Body.String())
	}
}

func TestPRHeadTrust_UnknownProjectAndBadBody(t *testing.T) {
	r, s := newPRHeadTrustRouter(t)

	// Admin PUT on an unknown project → 404 (typed, not opaque 500).
	if rr := doReqAs(r, http.MethodPut, "/api/v1/projects/nope/pr-head-config", `{"enabled":true}`, store.RoleAdmin); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown project PUT = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	// Admin PUT with malformed body → 400 (before any store write).
	if _, err := s.ApplyProject(context.Background(), store.ApplyProjectInput{Slug: "demo", Name: "demo"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if rr := doReqAs(r, http.MethodPut, "/api/v1/projects/demo/pr-head-config", `{bad`, store.RoleAdmin); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad json PUT = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}
