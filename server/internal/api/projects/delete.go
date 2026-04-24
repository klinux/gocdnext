package projects

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// DeleteProjectResponse echoes the blast radius of the cascading
// delete so the UI can confirm "deleted N pipelines, M runs, K
// secrets". Counts reflect what was under the project right
// before the delete — after the call, all child rows are gone
// via ON DELETE CASCADE.
type DeleteProjectResponse struct {
	Slug       string `json:"slug"`
	Pipelines  int64  `json:"pipelines_deleted"`
	Runs       int64  `json:"runs_deleted"`
	Secrets    int64  `json:"secrets_deleted"`
	SCMSources int64  `json:"scm_sources_deleted"`
}

// Delete handles DELETE /api/v1/projects/{slug}. The DB's
// ON DELETE CASCADE constraints fan the delete out to pipelines,
// materials, runs, artifacts, secrets and scm_sources, so this
// handler is a thin wrapper over store.DeleteProject.
//
// 404 when the slug doesn't match any project. 200 with the
// deletion-counts body on success — not 204, because the counts
// are useful for confirmation toasts and audit trails.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}

	counts, err := h.store.DeleteProject(r.Context(), slug)
	switch {
	case errors.Is(err, store.ErrProjectNotFound):
		http.Error(w, "project not found", http.StatusNotFound)
		return
	case err != nil:
		h.log.Error("delete project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.log.Info("delete project",
		"slug", slug,
		"pipelines", counts.Pipelines,
		"runs", counts.Runs,
		"secrets", counts.Secrets,
		"scm_sources", counts.SCMSources)

	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectDelete, "project", slug,
		map[string]any{
			"pipelines_deleted":   counts.Pipelines,
			"runs_deleted":        counts.Runs,
			"secrets_deleted":     counts.Secrets,
			"scm_sources_deleted": counts.SCMSources,
		})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(DeleteProjectResponse{
		Slug:       slug,
		Pipelines:  counts.Pipelines,
		Runs:       counts.Runs,
		Secrets:    counts.Secrets,
		SCMSources: counts.SCMSources,
	})
}
