package projects

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// checkReportingResponse is the wire shape for GET/PUT
// /api/v1/projects/{slug}/check-reporting. `mode` is one of
// both|check_run|commit_status; `default_mode` is echoed so the settings UI
// can label the default without hardcoding it.
type checkReportingResponse struct {
	Mode        string `json:"mode"`
	DefaultMode string `json:"default_mode"`
}

type checkReportingRequest struct {
	Mode string `json:"mode"`
}

// GetCheckReporting handles GET /api/v1/projects/{slug}/check-reporting —
// the current per-project GitHub check reporting mode for the settings UI.
func (h *Handler) GetCheckReporting(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	mode, err := h.store.GetProjectCheckReportingBySlug(r.Context(), slug)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("get check-reporting: lookup", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(checkReportingResponse{
		Mode:        mode,
		DefaultMode: store.CheckReportingBoth,
	})
}

// SetCheckReporting handles PUT /api/v1/projects/{slug}/check-reporting.
// Body: {"mode": "both"|"check_run"|"commit_status"}. The value is validated
// at the edge (fail-fast) AND by the DB CHECK constraint (defense in depth).
func (h *Handler) SetCheckReporting(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req checkReportingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !store.ValidCheckReportingMode(req.Mode) {
		http.Error(w, "mode must be one of: both, check_run, commit_status", http.StatusBadRequest)
		return
	}
	if err := h.store.SetProjectCheckReportingBySlug(r.Context(), slug, req.Mode); err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("set check-reporting", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.log.Info("project check-reporting updated", "slug", slug, "mode", req.Mode)
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectCheckReportingSet, "project", slug,
		map[string]any{"slug": slug, "mode": req.Mode})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(checkReportingResponse{
		Mode:        req.Mode,
		DefaultMode: store.CheckReportingBoth,
	})
}
