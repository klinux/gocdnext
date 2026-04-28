package projects

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// logArchiveSettingsResponse is the wire shape for both GET and PUT
// /api/v1/projects/{slug}/log-archive. `enabled` is null when the
// project inherits the global policy; true/false when an explicit
// override is set. `global_policy` + `has_artifact_backend` are
// echoed so the settings UI can render the resolved-state hint
// without an extra admin endpoint round-trip.
type logArchiveSettingsResponse struct {
	Enabled            *bool  `json:"enabled"`
	GlobalPolicy       string `json:"global_policy,omitempty"`
	HasArtifactBackend bool   `json:"has_artifact_backend"`
}

type logArchiveSettingsRequest struct {
	Enabled *bool `json:"enabled"`
}

// GetLogArchiveSettings handles GET /api/v1/projects/{slug}/log-archive.
// Returns the per-project override (null = inherit global) plus the
// global-policy + backend-availability hints the settings UI needs.
func (h *Handler) GetLogArchiveSettings(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	flag, err := h.store.GetProjectLogArchiveFlagBySlug(r.Context(), slug)
	if err != nil {
		h.log.Error("get log_archive: lookup", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(logArchiveSettingsResponse{
		Enabled:            flag,
		GlobalPolicy:       h.logArchivePolicy,
		HasArtifactBackend: h.hasArtifactBackend,
	})
}

// SetLogArchiveSettings handles PUT /api/v1/projects/{slug}/log-archive.
// Three valid bodies:
//
//	{"enabled": true}   -> always archive (override global)
//	{"enabled": false}  -> never archive  (override global)
//	{"enabled": null}   -> inherit the global policy
//
// The "off" half of the global policy still wins — a project can't
// re-enable archiving when GOCDNEXT_LOG_ARCHIVE=off cluster-wide
// (the EffectiveLogArchive resolver enforces this on the read
// path; the override is stored verbatim either way).
func (h *Handler) SetLogArchiveSettings(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req logArchiveSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.store.SetProjectLogArchiveFlagBySlug(r.Context(), slug, req.Enabled); err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("set log_archive", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.log.Info("project log_archive updated",
		"slug", slug, "enabled", req.Enabled)
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectLogArchiveSet, "project", slug,
		map[string]any{"slug": slug, "enabled": req.Enabled})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(logArchiveSettingsResponse{Enabled: req.Enabled})
}
