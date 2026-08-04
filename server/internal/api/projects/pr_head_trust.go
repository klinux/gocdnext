package projects

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// prHeadTrustResponse is the wire shape for GET/PUT
// /api/v1/projects/{slug}/pr-head-config. `enabled` is whether a same-repo PR
// runs its own `.gocdnext/` (head config) instead of the default-branch one.
type prHeadTrustResponse struct {
	Enabled bool `json:"enabled"`
}

type prHeadTrustRequest struct {
	Enabled bool `json:"enabled"`
}

// GetPRHeadTrust handles GET /api/v1/projects/{slug}/pr-head-config — the
// current per-project opt-in for the settings UI. Read is maintainer+ (it sits
// in the write group but exposes no secret).
func (h *Handler) GetPRHeadTrust(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	enabled, err := h.store.GetProjectTrustSameRepoPRConfigBySlug(r.Context(), slug)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("get pr-head-config: lookup", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(prHeadTrustResponse{Enabled: enabled})
}

// SetPRHeadTrust handles PUT /api/v1/projects/{slug}/pr-head-config.
// Body: {"enabled": true|false}.
//
// ADMIN ONLY — enabling this lets same-repo PR authors control the executable
// graph (jobs, images, agent profiles, OIDC, clusters, deploys) for that run,
// so the mutation is gated tighter than the surrounding maintainer routes and
// is audited. Fail-closed: no authenticated admin in context => 403.
func (h *Handler) SetPRHeadTrust(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	u, ok := authapi.UserFromContext(r.Context())
	if !ok || !store.RoleSatisfies(u.Role, store.RoleAdmin) {
		http.Error(w, "changing PR-head config trust requires admin", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req prHeadTrustRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.store.SetProjectTrustSameRepoPRConfigBySlug(r.Context(), slug, req.Enabled); err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("set pr-head-config", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.log.Info("project pr-head-config trust updated", "slug", slug, "enabled", req.Enabled)
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectPRHeadTrustSet, "project", slug,
		map[string]any{"slug": slug, "enabled": req.Enabled})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(prHeadTrustResponse{Enabled: req.Enabled})
}
