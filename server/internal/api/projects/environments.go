package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
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
	// ID / CreatedAt / UpdatedAt are OPTIONAL because a row can be
	// freeze-only: `environments` rows are lazy, so a pre-emptive freeze on an
	// env nothing has deployed to — or an orphan freeze left behind by
	// deleting a frozen env — has no environments row at all. Emitting a zero
	// UUID and a zero time for those would have the UI render
	// "00000000-0000-…" and "0001-01-01"; absent is honest, and
	// has_environment_row is the explicit discriminator the client gates on.
	ID          string     `json:"id,omitempty"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	// No omitempty: it is the discriminator, and a silently-absent `false`
	// would read as "unknown" on the client.
	HasEnvironmentRow bool `json:"has_environment_row"`
	// Full timeline size for this environment. Persisted on environments so
	// project pages can show counts without per-card COUNT(*) queries. Freeze-only
	// rows have no environment record, so they correctly report 0.
	TotalDeploys int64 `json:"total_deploys"`

	// Change-freeze (00078). `frozen` and `frozen_at` are VIEWER-readable —
	// "production is frozen" is operational state everyone needs. `freeze_reason`
	// and `frozen_by` are not: a reason routinely carries incident detail
	// ("frozen: PCI audit finding INC-4412"), so they are redacted below
	// maintainer, mirroring how /deploy-watches redacts its config fields.
	Frozen       bool       `json:"frozen"`
	FrozenAt     *time.Time `json:"frozen_at,omitempty"`
	FrozenBy     string     `json:"frozen_by,omitempty"`
	FreezeReason string     `json:"freeze_reason,omitempty"`

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
	Total       int64           `json:"total"`
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

	// showFreezeDetail defaults true so an auth-disabled deployment (no user in
	// context) sees everything — same sensitive-default as ListDeployWatches.
	// With auth on, RequireAuth guarantees a user and the role gates it.
	showFreezeDetail := true
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		showFreezeDetail = store.RoleSatisfies(u.Role, store.RoleMaintainer)
	}

	out := make([]environmentDTO, 0, len(envs))
	for _, e := range envs {
		dto := environmentDTO{
			Name:              e.Name,
			Description:       e.Description,
			CreatedAt:         e.CreatedAt,
			UpdatedAt:         e.UpdatedAt,
			HasEnvironmentRow: e.HasEnvironmentRow,
			TotalDeploys:      e.TotalDeploys,
			Frozen:            e.Frozen,
			FrozenAt:          e.FrozenAt,
		}
		if e.ID != nil {
			dto.ID = e.ID.String()
		}
		if showFreezeDetail {
			dto.FrozenBy = e.FrozenBy
			dto.FreezeReason = e.FreezeReason
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
	total, ok, err := h.store.EnvironmentDeploymentTotal(r.Context(), detail.Project.ID, envID)
	if err != nil {
		h.log.Error("list deployments: scope/total", "slug", slug, "env_id", envID, "err", err)
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
	_ = json.NewEncoder(w).Encode(deploymentsListResponse{Deployments: out, Total: total})
}

type rollbackRequest struct {
	ToRevisionID string `json:"to_revision_id"`
}

// RollbackEnvironment handles
// POST /api/v1/projects/{slug}/environments/{envID}/rollback with
// body {to_revision_id} (#39 phase 3). Re-runs the deploy job of the
// target revision's run, flagged as a rollback — that run's immutable
// outputs re-resolve the SAME version, so the deploy ships it again
// and a fresh revision is recorded with is_rollback=true. Gated to
// maintainer+ at the router. Returns 202 (the re-dispatch is async).
func (h *Handler) RollbackEnvironment(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	envID, err := uuid.Parse(chi.URLParam(r, "envID"))
	if err != nil {
		http.Error(w, "malformed environment id", http.StatusBadRequest)
		return
	}
	var req rollbackRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	revID, err := uuid.Parse(req.ToRevisionID)
	if err != nil {
		http.Error(w, "malformed to_revision_id", http.StatusBadRequest)
		return
	}

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("rollback: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	triggeredBy := ""
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		triggeredBy = u.ID.String()
	}

	res, err := h.store.RollbackToRevision(r.Context(), store.RollbackInput{
		ProjectID:     detail.Project.ID,
		EnvironmentID: envID,
		RevisionID:    revID,
		TriggeredBy:   triggeredBy,
	})
	switch {
	case err == nil:
		audit.Emit(r.Context(), h.log, h.store,
			store.AuditActionDeployRollback, "environment", envID.String(),
			map[string]any{"slug": slug, "to_revision_id": revID.String(), "rerun_job_run_id": res.JobRunID.String()})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"job_run_id": res.JobRunID.String(),
			"run_id":     res.RunID.String(),
			"attempt":    res.Attempt,
		})
	case errors.Is(err, store.ErrEnvironmentNotFound),
		errors.Is(err, store.ErrRevisionNotFound),
		errors.Is(err, store.ErrRevisionWrongEnvironment):
		// All "the thing you named isn't here / isn't yours" → 404.
		http.Error(w, "environment or revision not found", http.StatusNotFound)
	case errors.Is(err, store.ErrRollbackNotSuccessful):
		http.Error(w, "can only roll back to a successful deploy", http.StatusUnprocessableEntity)
	case errors.Is(err, store.ErrRollbackRunGone):
		http.Error(w, "the target deploy's run was garbage-collected; cannot roll back to it", http.StatusUnprocessableEntity)
	case errors.Is(err, store.ErrEnvironmentFrozen):
		// A rollback is a deploy, so the change-freeze refuses it like any other
		// (#202). 409, not 403: nothing is wrong with the caller's permissions —
		// the environment's state is what conflicts, and it is temporary.
		http.Error(w, "this environment is frozen — lift the freeze before rolling back", http.StatusConflict)
	case errors.Is(err, store.ErrJobRunActive):
		http.Error(w, "the deploy job is still active — wait for it to finish", http.StatusConflict)
	default:
		h.log.Error("rollback", "slug", slug, "env_id", envID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// RedeployCurrentEnvironment handles
// POST /api/v1/projects/{slug}/environments/{envID}/redeploy. The server
// resolves the current successful deployment at request time and re-runs its
// deploy job as a normal redeploy, not as a rollback. Gated to maintainer+ at
// the router. Returns 202 (the re-dispatch is async).
func (h *Handler) RedeployCurrentEnvironment(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	envID, err := uuid.Parse(chi.URLParam(r, "envID"))
	if err != nil {
		http.Error(w, "malformed environment id", http.StatusBadRequest)
		return
	}

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("redeploy: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	triggeredBy := ""
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		triggeredBy = u.ID.String()
	}

	res, err := h.store.RedeployCurrent(r.Context(), store.RedeployCurrentInput{
		ProjectID:     detail.Project.ID,
		EnvironmentID: envID,
		TriggeredBy:   triggeredBy,
	})
	switch {
	case err == nil:
		audit.Emit(r.Context(), h.log, h.store,
			store.AuditActionDeployRedeploy, "environment", envID.String(),
			map[string]any{"slug": slug, "rerun_job_run_id": res.JobRunID.String()})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"job_run_id": res.JobRunID.String(),
			"run_id":     res.RunID.String(),
			"attempt":    res.Attempt,
		})
	case errors.Is(err, store.ErrEnvironmentNotFound):
		http.Error(w, "environment not found", http.StatusNotFound)
	case errors.Is(err, store.ErrRedeployNoCurrent):
		http.Error(w, "environment has no current deploy to redeploy", http.StatusUnprocessableEntity)
	case errors.Is(err, store.ErrRollbackRunGone):
		http.Error(w, "the current deploy's run was garbage-collected; cannot redeploy it", http.StatusUnprocessableEntity)
	case errors.Is(err, store.ErrEnvironmentFrozen):
		http.Error(w, "this environment is frozen — lift the freeze before redeploying", http.StatusConflict)
	case errors.Is(err, store.ErrJobRunActive):
		http.Error(w, "the deploy job is still active — wait for it to finish", http.StatusConflict)
	default:
		h.log.Error("redeploy", "slug", slug, "env_id", envID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// DeleteEnvironment handles DELETE /api/v1/projects/{slug}/environments/{envID}.
// Admin-only: the schema's ON DELETE CASCADE removes the environment's entire
// deploy history AND any registered target (including a gated one), so a
// maintainer must not be able to nuke a gated target's policy this way — it
// mirrors the gated-target SoD in DeleteDeployTarget. Route placement gates
// maintainer+; this handler tightens it to admin (auth-disabled => admin, like
// the rest of the deploy surface). Environments are lazy, so a later deploy to
// the same name re-creates it empty. 204 on delete, 404 if the env isn't in the
// project, 403 for non-admins.
func (h *Handler) DeleteEnvironment(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	envID, err := uuid.Parse(chi.URLParam(r, "envID"))
	if err != nil {
		http.Error(w, "malformed environment id", http.StatusBadRequest)
		return
	}
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		if !store.RoleSatisfies(u.Role, store.RoleAdmin) {
			http.Error(w, "removing an environment requires admin", http.StatusForbidden)
			return
		}
	}
	projectID, ok := h.resolveProjectID(w, r, slug)
	if !ok {
		return
	}
	outcome, err := h.store.DeleteEnvironment(r.Context(), projectID, envID)
	if err != nil {
		h.log.Error("delete environment", "slug", slug, "env_id", envID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	switch outcome {
	case store.EnvDeleteAbsent:
		http.Error(w, "environment not found", http.StatusNotFound)
		return
	case store.EnvDeleteActive:
		// Refuse rather than cascade a running deploy's revision+watch out from
		// under it (which would orphan the still-running job_run).
		http.Error(w, "environment has an active deploy — wait for it to finish or cancel it first", http.StatusConflict)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionEnvironmentDelete, "environment", envID.String(),
		map[string]any{"slug": slug})
	w.WriteHeader(http.StatusNoContent)
}
