// Package dashboard exposes the read-only endpoints the UI's home
// tab consumes: aggregate metrics, global runs timeline, agents
// list. Kept separate from `api/projects` + `api/runs` because
// these are composite queries (no single project/run owns them)
// and because we want a stable public shape for ops dashboards.
package dashboard

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

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

// RunsGlobal handles GET /api/v1/runs. Query params:
//   - limit (default 20, max 200)
//   - offset (default 0)
//   - status (optional filter, any domain.RunStatus value)
//   - cause (optional filter: webhook | pull_request | upstream | manual)
//   - project (optional project slug filter)
// Response includes `total` alongside the slice so the UI can
// render "N of M" without a second call.
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
	var offset int64
	if raw := r.URL.Query().Get("offset"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			http.Error(w, "invalid 'offset' query", http.StatusBadRequest)
			return
		}
		offset = parsed
	}
	filter := store.RunsFilter{
		Status:      r.URL.Query().Get("status"),
		Cause:       r.URL.Query().Get("cause"),
		ProjectSlug: r.URL.Query().Get("project"),
	}

	runs, err := h.store.ListRunsGlobal(r.Context(), limit, offset, filter)
	if err != nil {
		h.log.Error("list runs", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	total, err := h.store.CountRunsGlobal(r.Context(), filter)
	if err != nil {
		h.log.Error("count runs", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"runs":   runs,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
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

// AgentDetail handles GET /api/v1/agents/{id}. Returns the single
// agent's metadata + a tail of recent jobs it was assigned.
// Tail size from ?jobs= (default 50, max 200). Missing agent = 404.
func (h *Handler) AgentDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := chi.URLParam(r, "id")
	id, err := uuid.Parse(raw)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	jobsLimit := int32(50)
	if raw := r.URL.Query().Get("jobs"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 {
			http.Error(w, "invalid 'jobs' query", http.StatusBadRequest)
			return
		}
		if int32(parsed) > 200 {
			parsed = 200
		}
		jobsLimit = int32(parsed)
	}

	agent, err := h.store.FindAgentWithRunning(r.Context(), id, time.Now())
	if errors.Is(err, store.ErrAgentByIDNotFound) {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.log.Error("agent detail", "agent_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	jobs, err := h.store.ListJobsForAgent(r.Context(), id, jobsLimit)
	if err != nil {
		h.log.Error("agent jobs", "agent_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"agent": agent,
		"jobs":  jobs,
	})
}
