package runs

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Cancel handles POST /api/v1/runs/{id}/cancel.
// Response:
//   202 Accepted   — the run was active and is now canceled
//   404 Not Found  — unknown run id
//   409 Conflict   — run already in a terminal status
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	switch err := h.store.CancelRun(r.Context(), runID); {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"run_id": runID.String(), "status": "canceled"})
	case errors.Is(err, store.ErrRunNotFound):
		http.Error(w, "run not found", http.StatusNotFound)
	case errors.Is(err, store.ErrRunAlreadyTerminal):
		http.Error(w, "run already terminal", http.StatusConflict)
	default:
		h.log.Error("cancel run", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// rerunBody optional JSON body for Rerun. triggered_by lands on the
// new run row so the UI can show who asked. Omit it and the store
// synthesizes "rerun:<orig>" automatically.
type rerunBody struct {
	TriggeredBy string `json:"triggered_by,omitempty"`
}

// Rerun handles POST /api/v1/runs/{id}/rerun.
// Response on success (202):
//   { "run_id": "...", "counter": <int>, "rerun_of": "<orig>" }
// 422 when the original run's revisions can't be replayed (e.g.,
// modifications pruned, blank revisions JSON).
func (h *Handler) Rerun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	body, ok := decodeOptionalBody[rerunBody](w, r)
	if !ok {
		return
	}

	res, err := h.store.RerunRun(r.Context(), store.RerunRunInput{
		RunID:       runID,
		TriggeredBy: body.TriggeredBy,
	})
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id":   res.RunID.String(),
			"counter":  res.Counter,
			"rerun_of": runID.String(),
		})
	case errors.Is(err, store.ErrRunNotFound):
		http.Error(w, "run not found", http.StatusNotFound)
	case errors.Is(err, store.ErrNoModificationForPipeline),
		errors.Is(err, store.ErrRunRevisionsMissing):
		http.Error(w, "cannot replay this run: source revision is no longer available", http.StatusUnprocessableEntity)
	default:
		h.log.Error("rerun run", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

type triggerBody struct {
	TriggeredBy string `json:"triggered_by,omitempty"`
}

// TriggerPipeline handles POST /api/v1/pipelines/{id}/trigger.
// Picks the pipeline's latest modification and queues a run. 422 when
// the pipeline has never seen a push (no modifications to replay).
func (h *Handler) TriggerPipeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := chi.URLParam(r, "id")
	pipelineID, err := uuid.Parse(raw)
	if err != nil {
		http.Error(w, "invalid pipeline id", http.StatusBadRequest)
		return
	}
	body, ok := decodeOptionalBody[triggerBody](w, r)
	if !ok {
		return
	}

	res, err := h.store.TriggerManualRun(r.Context(), store.TriggerManualRunInput{
		PipelineID:  pipelineID,
		TriggeredBy: body.TriggeredBy,
	})
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id":      res.RunID.String(),
			"counter":     res.Counter,
			"pipeline_id": pipelineID.String(),
		})
	case errors.Is(err, store.ErrNoModificationForPipeline):
		http.Error(w, "pipeline has no modifications yet — push to a matched material first", http.StatusUnprocessableEntity)
	default:
		h.log.Error("trigger pipeline", "pipeline_id", pipelineID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// --- helpers shared by the action endpoints ---

func parseRunID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := chi.URLParam(r, "id")
	id, err := uuid.Parse(raw)
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}

// decodeOptionalBody parses a JSON body when present. Empty body is
// fine (returns zero-value T); non-empty but malformed is 400.
func decodeOptionalBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var out T
	if r.Body == nil {
		return out, true
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return out, false
	}
	if len(raw) == 0 {
		return out, true
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return out, false
	}
	return out, true
}
