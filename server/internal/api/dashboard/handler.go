// Package dashboard exposes the read-only endpoints the UI's home
// tab consumes: aggregate metrics, global runs timeline, agents
// list. Kept separate from `api/projects` + `api/runs` because
// these are composite queries (no single project/run owns them)
// and because we want a stable public shape for ops dashboards.
package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

const defaultRunsLimit int32 = 20
const maxRunsLimit int32 = 200

// Handler owns the dashboard routes. Registered on chi under
// `/api/v1/dashboard/*` and `/api/v1/agents`.
type Handler struct {
	store *store.Store
	log   *slog.Logger
}

func NewHandler(s *store.Store, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: s, log: log}
}

// Metrics handles GET /api/v1/dashboard/metrics. Cheap aggregate
// queries; safe to poll every few seconds.
func (h *Handler) Metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m, err := h.store.GetDashboardMetrics(r.Context())
	if err != nil {
		h.log.Error("dashboard metrics", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

// RunsGlobal handles GET /api/v1/dashboard/runs. Query params:
//   - limit (default 20, max 200)
//   - status (optional filter, any domain.RunStatus value)
func (h *Handler) RunsGlobal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := defaultRunsLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 {
			http.Error(w, "invalid 'limit' query", http.StatusBadRequest)
			return
		}
		if int32(parsed) > maxRunsLimit {
			parsed = int64(maxRunsLimit)
		}
		limit = int32(parsed)
	}
	status := r.URL.Query().Get("status")
	runs, err := h.store.ListRunsGlobal(r.Context(), limit, status)
	if err != nil {
		h.log.Error("dashboard runs", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs})
}

// Agents handles GET /api/v1/agents. Flat list for both the home
// widget and the dedicated /agents page (UI.3).
func (h *Handler) Agents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agents, err := h.store.ListAgentsWithRunning(r.Context(), time.Now())
	if err != nil {
		h.log.Error("agents list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"agents": agents})
}
