package projects_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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

// TestListSecrets_InheritedExcludesShadowAcrossPages: the inherited-globals
// panel must filter against ALL of the project's local names, not just the
// current page — otherwise a local secret that lands on a later page would let
// its global twin show up as "inherited" on page 1, which is a lie.
func TestListSecrets_InheritedExcludesShadowAcrossPages(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	c, err := crypto.NewCipherFromHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	ctx := t.Context()
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Globals: one shadowed by a local, one genuinely inherited.
	if _, err := s.SetGlobalSecret(ctx, c, store.SecretSet{Name: "ZZZ_SHADOW", Value: []byte("g1")}); err != nil {
		t.Fatalf("global 1: %v", err)
	}
	if _, err := s.SetGlobalSecret(ctx, c, store.SecretSet{Name: "GLB_ONLY", Value: []byte("g2")}); err != nil {
		t.Fatalf("global 2: %v", err)
	}
	// Locals: AAA/BBB sort first; ZZZ_SHADOW shadows the global but, under
	// ORDER BY name + limit=1, never appears on page 1.
	for _, n := range []string{"AAA", "BBB", "ZZZ_SHADOW"} {
		if _, err := s.SetSecret(ctx, c, store.SecretSet{ProjectID: applied.ProjectID, Name: n, Value: []byte("v")}); err != nil {
			t.Fatalf("local %s: %v", n, err)
		}
	}

	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))).WithCipher(c)
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/secrets", h.ListSecrets)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo/secrets?limit=1&offset=0", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Secrets   []store.Secret `json:"secrets"`
		Inherited []store.Secret `json:"inherited"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Secrets) != 1 || resp.Secrets[0].Name != "AAA" {
		t.Fatalf("page 1 = %+v, want exactly [AAA]", resp.Secrets)
	}
	inh := map[string]bool{}
	for _, g := range resp.Inherited {
		inh[g.Name] = true
	}
	if inh["ZZZ_SHADOW"] {
		t.Fatalf("inherited leaked a shadowed global not on the current page: %+v", resp.Inherited)
	}
	if !inh["GLB_ONLY"] {
		t.Fatalf("inherited dropped a genuinely-inherited global: %+v", resp.Inherited)
	}
}

// TestListSecrets_ConfiguredSourcesGatesDB: the server is authoritative on
// which sources a write may pick — db only when a cipher is configured, plus
// each enabled backend. An external-only deployment must not advertise db
// (the UI would offer it and the write would 503).
func TestListSecrets_ConfiguredSourcesGatesDB(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	if _, err := s.ApplyProject(t.Context(), store.ApplyProjectInput{Slug: "demo", Name: "Demo"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	sources := func(withCipher bool, ext []string) []string {
		h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if withCipher {
			c, _ := crypto.NewCipherFromHex(strings.Repeat("ab", 32))
			h = h.WithCipher(c)
		}
		h = h.WithSecretSources(ext)
		r := chi.NewRouter()
		r.Get("/api/v1/projects/{slug}/secrets", h.ListSecrets)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo/secrets", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		var resp struct {
			ConfiguredSources []string `json:"configured_sources"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
		return resp.ConfiguredSources
	}

	if got := sources(true, []string{"vault"}); !slices.Contains(got, "db") || !slices.Contains(got, "vault") {
		t.Fatalf("cipher+vault: configured_sources=%v, want db+vault", got)
	}
	got := sources(false, []string{"vault"})
	if slices.Contains(got, "db") {
		t.Fatalf("external-only: configured_sources=%v must not include db", got)
	}
	if !slices.Contains(got, "vault") {
		t.Fatalf("external-only: configured_sources=%v should include vault", got)
	}
}

// TestSetSecret_TrimsSourceAndRef: a stray copy-paste space in source/path/key
// must be trimmed server-side, so it can't silently become a different
// reference + cache key than what the operator intended.
func TestSetSecret_TrimsSourceAndRef(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	c, err := crypto.NewCipherFromHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	if _, err := s.ApplyProject(t.Context(), store.ApplyProjectInput{Slug: "demo", Name: "Demo"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithCipher(c).WithSecretSources([]string{"vault"})
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{slug}/secrets", h.SetSecret)
	r.Get("/api/v1/projects/{slug}/secrets", h.ListSecrets)

	rr := postSecret(t, r, "demo", map[string]any{
		"name":   "GH_TOKEN",
		"source": " vault ",
		"ref":    map[string]string{"path": "  secret/ci/gh  ", "key": " token "},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/demo/secrets", nil)
	gr := httptest.NewRecorder()
	r.ServeHTTP(gr, req)
	var resp struct {
		Secrets []store.Secret `json:"secrets"`
	}
	if err := json.Unmarshal(gr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Secrets) != 1 || resp.Secrets[0].Ref == nil {
		t.Fatalf("secrets = %+v", resp.Secrets)
	}
	if got := resp.Secrets[0]; got.Source != "vault" || got.Ref.Path != "secret/ci/gh" || got.Ref.Key != "token" {
		t.Fatalf("not trimmed: source=%q ref=%+v", got.Source, got.Ref)
	}
}
