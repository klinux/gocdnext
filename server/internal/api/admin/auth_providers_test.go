package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/auth"
	"github.com/gocdnext/gocdnext/server/internal/config"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func buildAuthCRUD(t *testing.T) (*adminapi.AuthProvidersHandler, *store.Store, *auth.Registry, http.Handler) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	s.SetAuthCipher(c)

	cfg := &config.Config{
		AuthEnabled: true,
		PublicBase:  "http://localhost:8153",
	}
	registry := auth.NewRegistry()
	h := adminapi.NewAuthProvidersHandler(s, registry, cfg, quietLogger())

	r := chi.NewRouter()
	r.Get("/api/v1/admin/auth/providers", h.List)
	r.Post("/api/v1/admin/auth/providers", h.Upsert)
	r.Delete("/api/v1/admin/auth/providers/{id}", h.Delete)
	r.Post("/api/v1/admin/auth/providers/reload", h.Reload)
	return h, s, registry, r
}

func TestAuthProvidersHandler_UpsertGitHubReloadsRegistry(t *testing.T) {
	_, _, registry, srv := buildAuthCRUD(t)

	body := map[string]any{
		"name":          "github",
		"kind":          "github",
		"display_name":  "GitHub",
		"client_id":     "gh-id",
		"client_secret": "gh-secret",
		"enabled":       true,
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/auth/providers", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	// Registry should now carry the freshly-created github provider.
	if registry.Get(auth.ProviderGitHub) == nil {
		t.Fatalf("registry did not pick up the new provider")
	}
	if registry.Len() != 1 {
		t.Fatalf("registry len = %d, want 1", registry.Len())
	}
}

func TestAuthProvidersHandler_List_IncludesConfiguredAndEnvOnly(t *testing.T) {
	_, s, registry, srv := buildAuthCRUD(t)

	// Seed a DB provider directly.
	if _, err := s.UpsertConfiguredProvider(context.Background(), store.UpsertAuthProviderInput{
		Name: "google", Kind: store.ProviderKindOIDC,
		DisplayName: "Google",
		ClientID:    "g-id", ClientSecret: "g-sec",
		Issuer:  "https://accounts.google.com",
		Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Pretend an env-only provider is registered (we don't run the
	// full BuildRegistry here; inject a stub directly).
	registry.Replace(stubRegistryProvider{name: "github", display: "GitHub"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/auth/providers", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var payload struct {
		Enabled   bool                          `json:"enabled"`
		Providers []store.ConfiguredProvider    `json:"providers"`
		EnvOnly   []string                      `json:"env_only"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &payload)

	if !payload.Enabled {
		t.Fatalf("enabled should be true")
	}
	if len(payload.Providers) != 1 || payload.Providers[0].Name != "google" {
		t.Fatalf("configured = %+v", payload.Providers)
	}
	if len(payload.EnvOnly) != 1 || payload.EnvOnly[0] != "github" {
		t.Fatalf("env_only = %+v", payload.EnvOnly)
	}
}

func TestAuthProvidersHandler_Delete(t *testing.T) {
	_, s, registry, srv := buildAuthCRUD(t)

	created, err := s.UpsertConfiguredProvider(context.Background(), store.UpsertAuthProviderInput{
		Name: "keycloak", Kind: store.ProviderKindOIDC,
		ClientID: "kc", ClientSecret: "s",
		Issuer: "https://kc/realms/r", Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Populate registry so the reload-after-delete path empties it.
	registry.Replace(stubRegistryProvider{name: "keycloak"})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/auth/providers/"+created.ID.String(), nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rr.Code)
	}
	// A second delete hits ErrAuthProviderNotFound → 404.
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/v1/admin/auth/providers/"+created.ID.String(), nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d", rr.Code)
	}
}

func TestAuthProvidersHandler_UpsertValidationFails(t *testing.T) {
	_, _, _, srv := buildAuthCRUD(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/auth/providers",
		bytes.NewReader([]byte(`{"name":"","kind":"github","client_id":"x","client_secret":"y"}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

type stubRegistryProvider struct {
	name    auth.ProviderName
	display string
}

func (s stubRegistryProvider) Name() auth.ProviderName { return s.name }
func (s stubRegistryProvider) DisplayName() string {
	if s.display == "" {
		return string(s.name)
	}
	return s.display
}
func (stubRegistryProvider) AuthorizeURL(string, string) string { return "https://idp/authorize" }
func (stubRegistryProvider) Exchange(context.Context, string, string, string) (auth.Claims, error) {
	return auth.Claims{}, nil
}
