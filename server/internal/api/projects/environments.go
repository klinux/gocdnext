package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// deploymentDTO is the JSON shape of one deployment_revision. run_id
// is a pointer because it goes NULL once the run is garbage-collected
// (the revision survives as an audit fact); the UI degrades the run
// link when it's absent.
type deploymentDTO struct {
	ID         string     `json:"id"`
	RunID      *string    `json:"run_id,omitempty"`
	Attempt    int32      `json:"attempt"`
	Version    string     `json:"version"`
	Status     string     `json:"status"`
	IsRollback bool       `json:"is_rollback"`
	DeployedBy string     `json:"deployed_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type environmentDTO struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// No omitempty: an environment with nothing deployed emits an
	// explicit "current": null so the TS contract (DeploymentRecord |
	// null) is stable rather than "field sometimes absent".
	Current *deploymentDTO `json:"current"`
}

type environmentsListResponse struct {
	Environments []environmentDTO `json:"environments"`
}

type deploymentsListResponse struct {
	Deployments []deploymentDTO `json:"deployments"`
}

// deploymentHistoryLimit caps the timeline page. Generous: the
// Environments tab shows a single env's history, and the index serves
// it newest-first off idx_deployment_revisions_history.
const deploymentHistoryLimit = 100

func toDeploymentDTO(r store.DeploymentRevision) deploymentDTO {
	d := deploymentDTO{
		ID:         r.ID.String(),
		Attempt:    r.Attempt,
		Version:    r.Version,
		Status:     r.Status,
		IsRollback: r.IsRollback,
		DeployedBy: r.DeployedBy,
		CreatedAt:  r.CreatedAt,
		FinishedAt: r.FinishedAt,
	}
	if r.RunID != nil {
		s := r.RunID.String()
		d.RunID = &s
	}
	return d
}

// ListEnvironments handles GET /api/v1/projects/{slug}/environments.
// Returns every environment with its current deployment (#39).
func (h *Handler) ListEnvironments(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("list environments: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	envs, err := h.store.ListEnvironmentsWithCurrent(r.Context(), detail.Project.ID)
	if err != nil {
		h.log.Error("list environments", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := make([]environmentDTO, 0, len(envs))
	for _, e := range envs {
		dto := environmentDTO{
			ID:          e.ID.String(),
			Name:        e.Name,
			Description: e.Description,
			CreatedAt:   e.CreatedAt,
			UpdatedAt:   e.UpdatedAt,
		}
		if e.Current != nil {
			cur := toDeploymentDTO(*e.Current)
			dto.Current = &cur
		}
		out = append(out, dto)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(environmentsListResponse{Environments: out})
}

// ListEnvironmentDeployments handles
// GET /api/v1/projects/{slug}/environments/{envID}/deployments — the
// timeline for one environment, newest first, all statuses (#39).
func (h *Handler) ListEnvironmentDeployments(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	envID, err := uuid.Parse(chi.URLParam(r, "envID"))
	if err != nil {
		http.Error(w, "malformed environment id", http.StatusBadRequest)
		return
	}
	// Resolve + scope-check: the environment must belong to the slug's
	// project, so a valid env id from another project can't be read
	// through this project's URL.
	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("list deployments: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ok, err := h.store.EnvironmentBelongsToProject(r.Context(), detail.Project.ID, envID)
	if err != nil {
		h.log.Error("list deployments: scope check", "slug", slug, "env_id", envID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "environment not found", http.StatusNotFound)
		return
	}

	limit := deploymentHistoryLimit
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n < limit {
			limit = n
		}
	}

	revs, err := h.store.ListDeploymentHistory(r.Context(), envID, int32(limit))
	if err != nil {
		h.log.Error("list deployments", "slug", slug, "env_id", envID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]deploymentDTO, 0, len(revs))
	for _, rev := range revs {
		out = append(out, toDeploymentDTO(rev))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(deploymentsListResponse{Deployments: out})
}
