package runs

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// pgTsPtr converts a pgtype.Timestamptz into a *time.Time so the
// JSON encoder emits null for not-yet-set columns (e.g. ready_at
// while a service is still starting) instead of "0001-01-01".
func pgTsPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

func uuidString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuid.UUID(id.Bytes).String()
}

// ServiceResponse is the wire shape the UI consumes per row in
// GET /api/v1/runs/{id}/services. Timestamps are *time.Time so
// the JSON encoder emits null for "not yet observed" (e.g.
// ready_at on a still-starting service) without forcing an
// "epoch" sentinel.
type ServiceResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Image     string     `json:"image"`
	PodName   string     `json:"pod_name,omitempty"`
	Status    string     `json:"status"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	ReadyAt   *time.Time `json:"ready_at,omitempty"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// Services handles GET /api/v1/runs/{id}/services. Returns every
// service_runs row keyed to the given run, alphabetically
// ordered. Empty array for runs without services or before any
// lifecycle event has arrived — never null, so the UI's
// `services.map(...)` works on first paint.
func (h *Handler) Services(w http.ResponseWriter, r *http.Request) {
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
	rows, err := h.store.ListServiceRunsByRunID(r.Context(), runID)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		h.log.Error("list service runs", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]ServiceResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, ServiceResponse{
			ID:        uuidString(row.ID),
			Name:      row.Name,
			Image:     row.Image,
			PodName:   row.PodName,
			Status:    row.Status,
			StartedAt: pgTsPtr(row.StartedAt),
			ReadyAt:   pgTsPtr(row.ReadyAt),
			StoppedAt: pgTsPtr(row.StoppedAt),
			Error:     row.Error,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
