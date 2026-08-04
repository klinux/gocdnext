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

// prHeadTrustRequest uses a *bool so a missing `enabled` (or a typo'd key like
// `enable`, rejected by DisallowUnknownFields) is a 400 rather than silently
// decoding to false — this is a security setting, not a preference.
type prHeadTrustRequest struct {
	Enabled *bool `json:"enabled"`
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
	// ADMIN ONLY. Match the codebase convention (deploy targets, environments):
	// enforce admin only when a user IS present in context. An auth-disabled
	// install has no user, and the existing contract treats that as the local
	// operator = admin — so the toggle stays changeable there rather than being
	// permanently un-settable.
	if u, ok := authapi.UserFromContext(r.Context()); ok && !store.RoleSatisfies(u.Role, store.RoleAdmin) {
		http.Error(w, "changing PR-head config trust requires admin", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req prHeadTrustRequest
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Enabled == nil {
		http.Error(w, "field 'enabled' (bool) is required", http.StatusBadRequest)
		return
	}
	if dec.More() {
		http.Error(w, "unexpected trailing content after the JSON object", http.StatusBadRequest)
		return
	}
	enabled := *req.Enabled
	if err := h.store.SetProjectTrustSameRepoPRConfigBySlug(r.Context(), slug, enabled); err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("set pr-head-config", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.log.Info("project pr-head-config trust updated", "slug", slug, "enabled", enabled)
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectPRHeadTrustSet, "project", slug,
		map[string]any{"slug": slug, "enabled": enabled})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(prHeadTrustResponse{Enabled: enabled})
}
