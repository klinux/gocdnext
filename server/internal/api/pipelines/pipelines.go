// Package pipelines exposes read endpoints for pipeline entities that
// don't fit under /projects or /runs. Today it only surfaces the YAML
// view of a pipeline; more endpoints (export, duplicate, diff against
// repo) can land here without churning the runs handler.
package pipelines

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/parser"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

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

// YAML handles GET /api/v1/pipelines/{id}/yaml. Reconstructs the YAML
// from the stored domain.Pipeline via the parser emitter. The original
// on-disk YAML isn't persisted today — when it is (coming with the
// "Sync from repo" flow), this endpoint returns the verbatim source
// instead and the "reconstructed" banner in the UI goes away.
//
// Response shape:
//
//	{ "yaml": "...", "reconstructed": true }
//
// Keeping the flag in the payload lets the UI differentiate between
// "this is what you wrote" and "this is what we can reconstruct" so
// operators never mistake a round-tripped doc for their source.
func (h *Handler) YAML(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	p, err := h.store.GetPipelineByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrPipelineNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.log.Error("pipelines: load", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	buf, err := parser.Emit(p)
	if err != nil {
		h.log.Error("pipelines: emit yaml", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"yaml":          string(buf),
		"reconstructed": true,
	})
}
