package admin_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/config"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
)

func realPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	}))
}

func ptrInt64(v int64) *int64 { return &v }

func buildVCSAdmin(t *testing.T) (*adminapi.VCSIntegrationsHandler, *store.Store, *vcs.Registry, http.Handler) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	s.SetAuthCipher(c)

	cfg := &config.Config{PublicBase: "https://ci.example.com"}
	reg := vcs.New()
	h := adminapi.NewVCSIntegrationsHandler(s, reg, cfg, quietLogger())

	r := chi.NewRouter()
	r.Get("/api/v1/admin/integrations/vcs", h.List)
	r.Post("/api/v1/admin/integrations/vcs", h.Upsert)
	r.Delete("/api/v1/admin/integrations/vcs/{id}", h.Delete)
	r.Post("/api/v1/admin/integrations/vcs/reload", h.Reload)
	return h, s, reg, r
}

func TestVCSIntegrationsHandler_UpsertHotSwaps(t *testing.T) {
	_, _, registry, srv := buildVCSAdmin(t)

	body := map[string]any{
		"name":          "main-gh",
		"kind":          "github_app",
		"display_name":  "gocdnext",
		"app_id":        int64(123456),
		"private_key":   realPEM(t),
		"webhook_secret": "wh-secret",
		"api_base":      "",
		"enabled":       true,
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/vcs", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	// Registry should now carry an active GitHub App client.
	if registry.GitHubApp() == nil {
		t.Fatalf("registry didn't hot-swap to the new App after upsert")
	}
}

func TestVCSIntegrationsHandler_List(t *testing.T) {
	_, s, registry, srv := buildVCSAdmin(t)

	// Seed one DB row + stub a registry entry (env-flavored) so the
	// response carries both shapes.
	if _, err := s.UpsertVCSIntegration(context.Background(), store.UpsertVCSIntegrationInput{
		Name: "db-row", Kind: store.VCSKindGitHubApp,
		AppID: ptrInt64(42), PrivateKeyPEM: []byte(realPEM(t)),
		Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	registry.Replace(nil, []vcs.Integration{{
		Name: "env", Kind: "github_app", Enabled: true, Source: vcs.SourceEnv,
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/vcs", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var payload struct {
		Integrations []store.ConfiguredVCSIntegration `json:"integrations"`
		Active       []vcs.Integration                `json:"active"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &payload)
	if len(payload.Integrations) != 1 || payload.Integrations[0].Name != "db-row" {
		t.Fatalf("DB rows missing: %+v", payload.Integrations)
	}
	if len(payload.Active) != 1 || payload.Active[0].Source != vcs.SourceEnv {
		t.Fatalf("active payload missing env row: %+v", payload.Active)
	}
	if payload.Integrations[0].HasPrivateKey != true {
		t.Fatalf("HasPrivateKey flag should be true when a key is stored")
	}
}

func TestVCSIntegrationsHandler_Delete(t *testing.T) {
	_, s, registry, srv := buildVCSAdmin(t)

	created, err := s.UpsertVCSIntegration(context.Background(), store.UpsertVCSIntegrationInput{
		Name: "gone", Kind: store.VCSKindGitHubApp,
		AppID: ptrInt64(1), PrivateKeyPEM: []byte(realPEM(t)),
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Populate registry so we can observe the reload emptying it.
	registry.Replace(nil, []vcs.Integration{{Name: "gone", Kind: "github_app"}})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/integrations/vcs/"+created.ID.String(), nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rr.Code)
	}

	// Second delete → 404.
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/v1/admin/integrations/vcs/"+created.ID.String(), nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d", rr.Code)
	}
}

func TestVCSIntegrationsHandler_UpsertValidationFails(t *testing.T) {
	_, _, _, srv := buildVCSAdmin(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/vcs",
		bytes.NewReader([]byte(`{"name":"","kind":"github_app","app_id":1,"private_key":"..."}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
