package projects_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func rolloutGateRouter(t *testing.T) http.Handler {
	t.Helper()
	s := seedInflightWatch(t) // project "demo" exists (resolveProjectID succeeds)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{slug}/deploy-watches/{revID}/approve", h.ApproveRolloutGate)
	r.Post("/api/v1/projects/{slug}/deploy-watches/{revID}/reject", h.RejectRolloutGate)
	return r
}

func gatePost(r http.Handler, path, body, role string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if role != "" {
		req = req.WithContext(authapi.WithUser(req.Context(),
			store.User{ID: uuid.New(), Email: "u@example.com", Name: "U", Role: role}))
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func TestRolloutGate_HTTPGuards(t *testing.T) {
	r := rolloutGateRouter(t)
	rev := uuid.New().String()
	gate := `{"gate_id":"` + uuid.New().String() + `"}`

	t.Run("no auth -> 401", func(t *testing.T) {
		if rr := gatePost(r, "/api/v1/projects/demo/deploy-watches/"+rev+"/approve", gate, ""); rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
		}
	})
	t.Run("bad revID -> 400", func(t *testing.T) {
		if rr := gatePost(r, "/api/v1/projects/demo/deploy-watches/not-a-uuid/approve", gate, store.RoleViewer); rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
	})
	t.Run("missing gate_id -> 400", func(t *testing.T) {
		if rr := gatePost(r, "/api/v1/projects/demo/deploy-watches/"+rev+"/approve", `{}`, store.RoleViewer); rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
	})
	t.Run("unknown project -> 404", func(t *testing.T) {
		if rr := gatePost(r, "/api/v1/projects/nope/deploy-watches/"+rev+"/approve", gate, store.RoleViewer); rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
	})
	// Auth + a valid but nonexistent revision → the store returns ErrGateStale → 409.
	// Exercises the full handler→store wiring + the fault mapping.
	t.Run("nonexistent gate -> 409 stale", func(t *testing.T) {
		rr := gatePost(r, "/api/v1/projects/demo/deploy-watches/"+rev+"/approve", gate, store.RoleViewer)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (stale); body=%s", rr.Code, rr.Body.String())
		}
	})
	// A service account authenticates but its ID isn't a users(id) — reject BEFORE any
	// insert (else an empty allow-list would FK-fail as a 500).
	t.Run("service account -> 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/demo/deploy-watches/"+rev+"/approve", strings.NewReader(gate))
		req = req.WithContext(authapi.WithUser(req.Context(), store.User{
			ID: uuid.New(), Name: "ci-bot", Provider: "service_account", Role: store.RoleMaintainer,
		}))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
		}
	})
}
