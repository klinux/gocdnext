// Package buildcache serves the Gradle HTTP build cache protocol
// (org.gradle.caching.http.HttpBuildCache) straight from gocdnext's artifact
// backend, so a project gets an entry-addressable remote build cache without
// running a separate cache server.
//
// The protocol is intentionally tiny: GET /{key} returns a cache entry or 404,
// PUT /{key} stores one. Keys are content hashes Gradle computes from task
// inputs, so entries are immutable and idempotent — two builds that produce the
// same key write identical bytes, and distinct keys never collide. That removes
// the whole-blob last-writer-wins problem of the directory cache for Gradle's
// additive build cache, and lets concurrent branches/PRs share entries by key.
//
// Reads redirect to a signed object-store URL so entry bytes never stream
// through the server (the hot path stays off the control plane); writes (cache
// misses, far rarer and per-task small) proxy through. Auth is HTTP Basic — the
// password is the project build-cache token (what Gradle's `credentials {}`
// sends) — matched against projects.cache_token_hash.
package buildcache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	badgetoken "github.com/gocdnext/gocdnext/server/internal/badges"
	bctoken "github.com/gocdnext/gocdnext/server/internal/buildcache"
)

// cacheStore is the slice of *store.Store this handler needs: resolve a
// (project slug, static token hash) to the owning project id for the manual
// auth path, and a slug → id lookup for the minted (auto-wired) auth path.
type cacheStore interface {
	VerifyProjectCacheToken(ctx context.Context, slug, tokenHash string) (uuid.UUID, bool, error)
	// ProjectCacheAuthBySlug resolves a slug to its project id and whether the
	// build cache is currently enabled — used to authorize minted tokens AND to
	// make a disable revoke outstanding minted tokens immediately.
	ProjectCacheAuthBySlug(ctx context.Context, slug string) (uuid.UUID, bool, error)
}

// Handler serves the build cache protocol for a project.
type Handler struct {
	store        cacheStore
	blobs        artifacts.Store
	log          *slog.Logger
	maxEntrySize int64
	// signKey verifies minted (per-run, auto-wired) tokens. Empty disables the
	// minted path; the static project token still works.
	signKey []byte
}

// New builds the handler. maxEntrySize caps a single PUT body (0 = no cap);
// signKey verifies minted tokens (nil = manual-token-only). A nil blob store
// makes Mount a no-op at the call site (main only mounts when an artifact
// backend is configured).
func New(s cacheStore, blobs artifacts.Store, log *slog.Logger, maxEntrySize int64, signKey []byte) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: s, blobs: blobs, log: log, maxEntrySize: maxEntrySize, signKey: signKey}
}

// Mount registers the two protocol routes. The trailing-segment {key} is the
// Gradle cache key; chi treats it as a single path segment so it cannot contain
// a slash, and validateKey rejects anything but bounded lowercase hex anyway.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/api/v1/buildcache/{slug}/{key}", h.Get)
	r.Put("/api/v1/buildcache/{slug}/{key}", h.Put)
}

// Get streams the entry bytes back on a hit, or 404 on a miss — Gradle treats a
// 404 as "not cached" and proceeds. Note: the entry is PROXIED through the
// server (not a redirect) because Gradle's HttpBuildCache client rejects HTTP
// redirects ("Redirects are unsupported by the build cache"). The handler is
// stateless, so scale reads horizontally with replicas.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	auth, ok := h.authAndKey(w, r)
	if !ok {
		return
	}
	storageKey := storageKeyFor(auth.projectID, auth.key)

	rc, err := h.blobs.Get(r.Context(), storageKey)
	if err != nil {
		if errors.Is(err, artifacts.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.log.Error("buildcache get", "err", err, "storage_key", storageKey)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are already sent; just log. Gradle sees a truncated body and
		// treats the entry as a miss.
		h.log.Warn("buildcache get: stream", "err", err, "storage_key", storageKey)
	}
}

// Put stores a cache entry. Requires a WRITE credential — a read-only (minted
// for a PR/branch) token is rejected with 403, so untrusted runs can neither
// poison the shared cache nor exhaust its storage. The body is bounded by
// maxEntrySize and streamed to the backend (never fully buffered).
func (h *Handler) Put(w http.ResponseWriter, r *http.Request) {
	auth, ok := h.authAndKey(w, r)
	if !ok {
		return
	}
	if !auth.canWrite {
		http.Error(w, "read-only cache credential", http.StatusForbidden)
		return
	}
	storageKey := storageKeyFor(auth.projectID, auth.key)

	body := r.Body
	if h.maxEntrySize > 0 {
		body = http.MaxBytesReader(w, r.Body, h.maxEntrySize)
	}
	if _, err := h.blobs.Put(r.Context(), storageKey, body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "entry too large", http.StatusRequestEntityTooLarge)
			return
		}
		h.log.Error("buildcache put: store", "err", err, "storage_key", storageKey)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// authResult is the outcome of authenticating + validating a request.
type authResult struct {
	projectID uuid.UUID
	key       string
	canWrite  bool
}

// authAndKey verifies the Basic-auth token against the project and validates the
// cache key. On any failure it writes the response and returns ok=false. A 401
// carries WWW-Authenticate so a build configured with credentials retries.
func (h *Handler) authAndKey(w http.ResponseWriter, r *http.Request) (authResult, bool) {
	slug := chi.URLParam(r, "slug")
	key := chi.URLParam(r, "key")
	if !validKey(key) {
		http.Error(w, "invalid cache key", http.StatusBadRequest)
		return authResult{}, false
	}
	_, token, hasAuth := r.BasicAuth()
	if !hasAuth {
		h.unauthorized(w)
		return authResult{}, false
	}

	projectID, canWrite, ok, err := h.resolveToken(r, slug, token)
	if err != nil {
		h.log.Error("buildcache auth: verify", "err", err, "slug", slug)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return authResult{}, false
	}
	if !ok {
		h.unauthorized(w)
		return authResult{}, false
	}
	return authResult{projectID: projectID, key: key, canWrite: canWrite}, true
}

// resolveToken authenticates a credential to a project and reports whether it
// may WRITE (push). Two shapes:
//   - a MINTED token (bc1. prefix): stateless HMAC verify → (project id, write).
//     The slug must resolve to that same project AND still have the cache
//     enabled — so a token minted for project A can't be replayed on B, and
//     disabling the cache immediately revokes outstanding minted tokens.
//   - a STATIC project token (43-char base64url): hashed and matched against
//     projects.cache_token_hash for the slug; it's the operator's trusted
//     credential, so it carries write access.
func (h *Handler) resolveToken(r *http.Request, slug, token string) (uuid.UUID, bool, bool, error) {
	if bctoken.LooksLikeMinted(token) {
		mintedID, write, ok := bctoken.VerifyMintedToken(token, h.signKey, time.Now())
		if !ok {
			return uuid.Nil, false, false, nil
		}
		slugID, enabled, err := h.store.ProjectCacheAuthBySlug(r.Context(), slug)
		if err != nil {
			// A real DB error must surface as 500, not a silent 401 — otherwise
			// an outage reads as "bad token" and hides the fault.
			return uuid.Nil, false, false, err
		}
		if !enabled || slugID != mintedID {
			// Unknown slug, disabled cache, or project mismatch → the same
			// indistinguishable auth failure.
			return uuid.Nil, false, false, nil
		}
		return mintedID, write, true, nil
	}
	if !badgetoken.LooksLikeToken(token) {
		return uuid.Nil, false, false, nil
	}
	id, ok, err := h.store.VerifyProjectCacheToken(r.Context(), slug, badgetoken.HashToken(token))
	return id, true, ok, err
}

func (h *Handler) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="gocdnext-buildcache"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// storageKeyFor namespaces entries by project so a token for project A can never
// read or overwrite project B's entries. The key is validated lowercase hex, so
// the path has no traversal surface.
func storageKeyFor(projectID uuid.UUID, key string) string {
	return "gradle-cache/" + projectID.String() + "/" + key
}

// validKey accepts only bounded lowercase hex — the shape of a Gradle build
// cache key (a HashCode hex string). This bounds the storage path and rejects
// path traversal, slashes, and oversized keys.
func validKey(key string) bool {
	if len(key) < 16 || len(key) > 128 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
