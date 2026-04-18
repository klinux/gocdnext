package projects_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func secretsRouter(t *testing.T, withCipher bool) (http.Handler, *store.Store) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if withCipher {
		c, err := crypto.NewCipherFromHex(strings.Repeat("ab", 32))
		if err != nil {
			t.Fatalf("cipher: %v", err)
		}
		h = h.WithCipher(c)
	}

	// Seed a project so the handlers have something to point at.
	if _, err := s.ApplyProject(t.Context(), store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	r := chi.NewRouter()
	r.Post("/api/v1/projects/{slug}/secrets", h.SetSecret)
	r.Get("/api/v1/projects/{slug}/secrets", h.ListSecrets)
	r.Delete("/api/v1/projects/{slug}/secrets/{name}", h.DeleteSecret)
	return r, s
}

func postSecret(t *testing.T, router http.Handler, slug string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+slug+"/secrets", &buf)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestSetSecret_Created(t *testing.T) {
	router, _ := secretsRouter(t, true)
	rr := postSecret(t, router, "demo", map[string]string{"name": "GH_TOKEN", "value": "ghp_xyz"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSetSecret_UpsertReturns200(t *testing.T) {
	router, _ := secretsRouter(t, true)
	if rr := postSecret(t, router, "demo", map[string]string{"name": "X", "value": "v1"}); rr.Code != http.StatusCreated {
		t.Fatalf("first: %d", rr.Code)
	}
	rr := postSecret(t, router, "demo", map[string]string{"name": "X", "value": "v2"})
	if rr.Code != http.StatusOK {
		t.Fatalf("second: %d (want 200 on update)", rr.Code)
	}
}

func TestSetSecret_BadName(t *testing.T) {
	router, _ := secretsRouter(t, true)
	rr := postSecret(t, router, "demo", map[string]string{"name": "has-dash", "value": "v"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestSetSecret_UnknownProject404(t *testing.T) {
	router, _ := secretsRouter(t, true)
	rr := postSecret(t, router, "nope", map[string]string{"name": "X", "value": "v"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestSetSecret_NoCipher503(t *testing.T) {
	router, _ := secretsRouter(t, false)
	rr := postSecret(t, router, "demo", map[string]string{"name": "X", "value": "v"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestListSecrets_NamesOnly(t *testing.T) {
	router, _ := secretsRouter(t, true)
	_ = postSecret(t, router, "demo", map[string]string{"name": "A", "value": "va"})
	_ = postSecret(t, router, "demo", map[string]string{"name": "B", "value": "vb"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo/secrets", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp struct {
		Secrets []store.Secret `json:"secrets"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Secrets) != 2 {
		t.Fatalf("secrets = %+v", resp.Secrets)
	}
	// Body must not leak values — assert no secret value appears literally.
	if strings.Contains(rr.Body.String(), "va") || strings.Contains(rr.Body.String(), "vb") {
		t.Fatalf("value leaked in list response: %s", rr.Body.String())
	}
}

func TestDeleteSecret_Removes(t *testing.T) {
	router, _ := secretsRouter(t, true)
	_ = postSecret(t, router, "demo", map[string]string{"name": "X", "value": "v"})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/demo/secrets/X", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rr.Code)
	}
	// Second delete must be 404.
	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("status on missing = %d", rr2.Code)
	}
}
