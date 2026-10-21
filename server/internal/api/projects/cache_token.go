package projects

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	badgetoken "github.com/gocdnext/gocdnext/server/internal/badges"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Management endpoints for the embedded Gradle build cache token
// (projects.cache_token_hash, migration 00094). The plaintext token is
// returned once on rotation and only its hash is persisted — same model as the
// badge token. Routes are maintainer+ (wired in main alongside the badge
// routes). The build cache endpoint itself lives in internal/api/buildcache.

type cacheConfigResponse struct {
	Enabled bool `json:"enabled"`
}

type cacheRotateResponse struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token"`
	// CacheURL is the base URL to set as the Gradle HttpBuildCache `url`.
	CacheURL string `json:"cache_url"`
	// GradleSnippet is a copy-pasteable settings.gradle(.kts) block.
	GradleSnippet string `json:"gradle_snippet"`
}

// GetCacheConfig handles GET /api/v1/projects/{slug}/build-cache.
func (h *Handler) GetCacheConfig(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	enabled, err := h.store.GetProjectCacheEnabledBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrProjectNotFound) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.log.Error("get cache config", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cacheConfigResponse{Enabled: enabled})
}

// RotateCacheToken handles POST /api/v1/projects/{slug}/build-cache/token.
// The plaintext token is returned once; only its hash is stored.
func (h *Handler) RotateCacheToken(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	token, err := badgetoken.NewToken()
	if err != nil {
		h.log.Error("rotate cache token: random", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := h.store.SetProjectCacheTokenHashBySlug(r.Context(), slug, badgetoken.HashToken(token)); err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("rotate cache token", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	cacheURL := h.cacheURL(slug)
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectCacheTokenRotate, "project", slug,
		map[string]any{"slug": slug})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cacheRotateResponse{
		Enabled:       true,
		Token:         token,
		CacheURL:      cacheURL,
		GradleSnippet: gradleSnippet(cacheURL),
	})
}

// DisableCacheToken handles DELETE /api/v1/projects/{slug}/build-cache/token.
func (h *Handler) DisableCacheToken(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	if err := h.store.ClearProjectCacheTokenBySlug(r.Context(), slug); err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("disable cache token", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectCacheTokenDisable, "project", slug,
		map[string]any{"slug": slug})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cacheConfigResponse{Enabled: false})
}

// cacheURL is the base URL the build points Gradle's HttpBuildCache at. The
// trailing slash matters: Gradle appends the entry key to it.
func (h *Handler) cacheURL(slug string) string {
	path := "/api/v1/buildcache/" + slug + "/"
	if h.publicBase == "" {
		return path
	}
	return h.publicBase + path
}

// gradleSnippet is a copy-pasteable settings.gradle.kts buildCache block for
// MANUAL (non-plugin) builds. It reads the token from an env var rather than
// embedding it — the plaintext token is returned once in the `token` field for
// the operator to store as a CI secret (GOCDNEXT_CACHE_TOKEN), never committed.
// Uses the Kotlin DSL property name `isPush` (Groovy's is `push`).
func gradleSnippet(cacheURL string) string {
	return "buildCache {\n" +
		"    remote<HttpBuildCache> {\n" +
		"        url = uri(\"" + cacheURL + "\")\n" +
		"        isPush = true\n" +
		"        credentials {\n" +
		"            username = \"gocdnext\"\n" +
		"            // Set GOCDNEXT_CACHE_TOKEN to the token shown above (store it as a CI secret).\n" +
		"            password = System.getenv(\"GOCDNEXT_CACHE_TOKEN\") ?: \"\"\n" +
		"        }\n" +
		"    }\n" +
		"}"
}
