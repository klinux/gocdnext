package projects_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	badgetoken "github.com/gocdnext/gocdnext/server/internal/badges"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newCacheTokenRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithPublicBase("https://ci.example.com")
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/build-cache", h.GetCacheConfig)
	r.Post("/api/v1/projects/{slug}/build-cache/token", h.RotateCacheToken)
	r.Delete("/api/v1/projects/{slug}/build-cache/token", h.DisableCacheToken)
	return r, s
}

func TestProjectCacheTokenRotateReturnedOnceAndStoredHashed(t *testing.T) {
	r, s := newCacheTokenRouter(t)
	ctx := context.Background()
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "demo"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Disabled before rotation.
	if rr := doReq(r, http.MethodGet, "/api/v1/projects/demo/build-cache", ""); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `"enabled":false`) {
		t.Fatalf("GET before = %d %s", rr.Code, rr.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/api/v1/projects/demo/build-cache/token", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Enabled       bool   `json:"enabled"`
		Token         string `json:"token"`
		CacheURL      string `json:"cache_url"`
		GradleSnippet string `json:"gradle_snippet"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Enabled || !badgetoken.LooksLikeToken(resp.Token) {
		t.Fatalf("response = %+v, want enabled + fresh token", resp)
	}
	if resp.CacheURL != "https://ci.example.com/api/v1/buildcache/demo/" {
		t.Errorf("cache_url = %q", resp.CacheURL)
	}
	if !strings.Contains(resp.GradleSnippet, resp.CacheURL) ||
		!strings.Contains(resp.GradleSnippet, "HttpBuildCache") ||
		!strings.Contains(resp.GradleSnippet, "isPush") {
		t.Errorf("snippet missing url/block/isPush: %q", resp.GradleSnippet)
	}
	// The snippet must NOT embed the plaintext token (it reads from env).
	if strings.Contains(resp.GradleSnippet, resp.Token) {
		t.Errorf("snippet embeds the plaintext token: %q", resp.GradleSnippet)
	}

	// Only the HASH is stored: the returned plaintext verifies, a wrong one does not.
	if _, ok, err := s.VerifyProjectCacheToken(ctx, "demo", badgetoken.HashToken(resp.Token)); err != nil || !ok {
		t.Fatalf("stored token does not verify: ok=%v err=%v", ok, err)
	}
	if enabled, _ := s.GetProjectCacheEnabledBySlug(ctx, "demo"); !enabled {
		t.Error("not enabled after rotate")
	}

	// Disable clears it.
	if rr := doReq(r, http.MethodDelete, "/api/v1/projects/demo/build-cache/token", ""); rr.Code != http.StatusOK {
		t.Fatalf("DELETE = %d", rr.Code)
	}
	if _, ok, _ := s.VerifyProjectCacheToken(ctx, "demo", badgetoken.HashToken(resp.Token)); ok {
		t.Error("token still verifies after disable")
	}
}

func TestProjectCacheTokenUnknownProject(t *testing.T) {
	r, _ := newCacheTokenRouter(t)
	if rr := doReq(r, http.MethodPost, "/api/v1/projects/ghost/build-cache/token", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("POST unknown = %d, want 404", rr.Code)
	}
}
