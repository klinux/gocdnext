package runs

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Coverage handles GET /api/v1/runs/{id}/coverage — every job's
// coverage summary for one run. The store query is run-scoped (one
// indexed lookup), no run-detail walk needed.
func (h *Handler) Coverage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	rows, err := h.store.CoverageByRun(r.Context(), runID)
	if err != nil {
		h.log.Error("coverage: by run", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"coverage": rows})
}

// CoverageTrend handles GET /api/v1/pipelines/{id}/coverage-trend
// — the newest N points for the pipeline sparkline (?limit=,
// default 50, capped at 200 store-side).
func (h *Handler) CoverageTrend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pipelineID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	points, err := h.store.CoverageTrend(r.Context(), pipelineID, int32(limit))
	if err != nil {
		h.log.Error("coverage: trend", "pipeline_id", pipelineID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"points": points})
}
