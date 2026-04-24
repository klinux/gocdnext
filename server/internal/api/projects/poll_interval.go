package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/parser"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// pollIntervalRequest is the wire shape for PUT
// /api/v1/projects/{slug}/poll-interval. `interval` is a Go
// duration string ("5m", "1h30m"); empty string disables polling
// (stored as zero nanoseconds).
type pollIntervalRequest struct {
	Interval string `json:"interval"`
}

// pollIntervalResponse echoes back the saved value plus a
// human-readable flag for the UI.
type pollIntervalResponse struct {
	Interval string `json:"interval"`
	Enabled  bool   `json:"enabled"`
}

// SetPollInterval handles PUT /api/v1/projects/{slug}/poll-interval.
// Sets the scm_source-level poll fallback that applies to the
// synthesized implicit project material. Per-material
// poll_interval on a declared git material in YAML overrides this.
// Empty string disables polling.
func (h *Handler) SetPollInterval(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req pollIntervalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Reuse the YAML parser's bounds check so UI + YAML reject
	// the exact same values. Empty is explicit disable (returns 0).
	interval, err := parser.ParsePollInterval(req.Interval)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	scm, err := h.store.FindSCMSourceByProjectSlug(r.Context(), slug)
	if err != nil {
		if errors.Is(err, store.ErrSCMSourceNotFound) {
			http.Error(w, "project has no scm_source bound — connect a repo first", http.StatusConflict)
			return
		}
		h.log.Error("set poll_interval: load scm_source", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.store.UpdateSCMSourcePollInterval(r.Context(), scm.ID, interval); err != nil {
		h.log.Error("set poll_interval", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.log.Info("project poll_interval updated",
		"slug", slug, "interval", interval)
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectPollSet, "project", slug,
		map[string]any{"slug": slug, "interval_ns": int64(interval)})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pollIntervalResponse{
		Interval: formatInterval(interval),
		Enabled:  interval > 0,
	})
}

func formatInterval(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}
