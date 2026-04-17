// Package runs exposes read-only HTTP endpoints for runs. The POST side (retry,
// cancel, manual trigger) will land in a later slice.
package runs

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

const defaultLogsPerJob int32 = 200

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

// Detail handles GET /api/v1/runs/{id}. Returns the run, its stages and jobs,
// plus a tail of log lines per job controlled by the `logs` query param
// (default 200, max 2000, 0 disables logs).
func (h *Handler) Detail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := chi.URLParam(r, "id")
	runID, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	logsPerJob := defaultLogsPerJob
	if raw := r.URL.Query().Get("logs"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 0 {
			http.Error(w, "invalid 'logs' query", http.StatusBadRequest)
			return
		}
		if parsed > 2000 {
			parsed = 2000
		}
		logsPerJob = int32(parsed)
	}

	detail, err := h.store.GetRunDetail(r.Context(), runID, logsPerJob)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		h.log.Error("get run detail", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(detail)
}
