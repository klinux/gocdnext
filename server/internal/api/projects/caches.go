package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// cacheDTO is the JSON shape surfaced to the UI. Kept separate
// from store.Cache so a future column on the DB row (tags,
// ttl-override, whatever) doesn't leak through the API without
// an explicit opt-in. Timestamps serialise as RFC3339 strings
// because that's what the RSC + RelativeTime helpers consume.
type cacheDTO struct {
	ID             string    `json:"id"`
	Key            string    `json:"key"`
	SizeBytes      int64     `json:"size_bytes"`
	Status         string    `json:"status"`
	ContentSHA256  string    `json:"content_sha256,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	LastAccessedAt time.Time `json:"last_accessed_at"`
}

type cachesListResponse struct {
	Caches     []cacheDTO `json:"caches"`
	TotalBytes int64      `json:"total_bytes"` // sum of `ready` rows only
}

// ListCaches handles GET /api/v1/projects/{slug}/caches.
// Returns every cache row owned by the project (ready + pending
// both visible so the operator sees stuck uploads in the same
// table as live keys), plus a total_bytes for the ready rows so
// the UI can show the effective footprint at the top.
func (h *Handler) ListCaches(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if errors.Is(err, store.ErrProjectNotFound) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.log.Error("caches list: project lookup", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rows, err := h.store.ListCachesByProject(r.Context(), detail.Project.ID)
	if err != nil {
		h.log.Error("caches list: store", "project", detail.Project.ID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := cachesListResponse{Caches: make([]cacheDTO, 0, len(rows))}
	for _, c := range rows {
		resp.Caches = append(resp.Caches, cacheDTO{
			ID:             c.ID.String(),
			Key:            c.Key,
			SizeBytes:      c.SizeBytes,
			Status:         c.Status,
			ContentSHA256:  c.ContentSHA256,
			CreatedAt:      c.CreatedAt,
			UpdatedAt:      c.UpdatedAt,
			LastAccessedAt: c.LastAccessedAt,
		})
		if c.Status == "ready" {
			resp.TotalBytes += c.SizeBytes
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// PurgeCache handles DELETE /api/v1/projects/{slug}/caches/{id}.
// Blob + row deletion in that order: the reverse would orphan
// the blob if the DB delete failed. 404 covers both "id unknown"
// and "id belongs to a different project" — the ownership check
// is a single query against (project_id, id).
func (h *Handler) PurgeCache(w http.ResponseWriter, r *http.Request) {
	if h.artifactStore == nil {
		http.Error(w, "artifact backend not configured", http.StatusServiceUnavailable)
		return
	}
	slug := chi.URLParam(r, "slug")
	idStr := chi.URLParam(r, "id")
	if slug == "" || idStr == "" {
		http.Error(w, "slug and id are required", http.StatusBadRequest)
		return
	}
	cacheID, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "malformed id", http.StatusBadRequest)
		return
	}

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if errors.Is(err, store.ErrProjectNotFound) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.log.Error("cache purge: project lookup", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	c, err := h.store.GetCacheForProject(r.Context(), detail.Project.ID, cacheID)
	if errors.Is(err, store.ErrCacheNotFound) {
		http.Error(w, "cache not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.log.Error("cache purge: row lookup", "project", detail.Project.ID, "id", cacheID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Blob first. ErrNotFound is benign (another sweeper beat us,
	// or the row never had a blob because it was still pending) —
	// treat as success and proceed to delete the row.
	if err := h.artifactStore.Delete(r.Context(), c.StorageKey); err != nil && !errors.Is(err, artifacts.ErrNotFound) {
		h.log.Error("cache purge: storage delete",
			"storage_key", c.StorageKey, "err", err)
		http.Error(w, "failed to delete cache blob", http.StatusInternalServerError)
		return
	}
	if err := h.store.DeleteCacheRow(r.Context(), c.ID); err != nil {
		h.log.Error("cache purge: row delete", "id", c.ID, "err", err)
		http.Error(w, "failed to delete cache row", http.StatusInternalServerError)
		return
	}

	h.log.Info("cache purged",
		"project", detail.Project.ID, "slug", slug,
		"cache_id", c.ID, "key", c.Key, "bytes", c.SizeBytes)
	w.WriteHeader(http.StatusNoContent)
}
