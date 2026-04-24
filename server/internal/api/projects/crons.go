package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	robfig "github.com/robfig/cron/v3"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/cron"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// projectCronDTO is the wire shape for one schedule. Field names
// mirror the table columns so the UI reads consistently against
// the SQL source of truth.
type projectCronDTO struct {
	ID           string     `json:"id"`
	ProjectID    string     `json:"project_id"`
	Name         string     `json:"name"`
	Expression   string     `json:"expression"`
	PipelineIDs  []string   `json:"pipeline_ids"`
	Enabled      bool       `json:"enabled"`
	LastFiredAt  *time.Time `json:"last_fired_at,omitempty"`
	CreatedBy    string     `json:"created_by,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type projectCronsResponse struct {
	Crons []projectCronDTO `json:"crons"`
}

type projectCronWriteRequest struct {
	Name        string   `json:"name"`
	Expression  string   `json:"expression"`
	PipelineIDs []string `json:"pipeline_ids"`
	Enabled     bool     `json:"enabled"`
}

const maxProjectCronBytes = 8 << 10 // 8 KiB

// cronParser matches the ticker's config so validation rejects
// any expression the ticker would also refuse. Keep in sync with
// cron.NewProject + cron.New.
var cronParser = robfig.NewParser(
	robfig.Minute | robfig.Hour | robfig.Dom |
		robfig.Month | robfig.Dow | robfig.Descriptor,
)

// ListProjectCrons handles GET /api/v1/projects/{slug}/crons.
// Returns the full list (enabled + disabled) so the UI can render
// the on/off toggle state without a second call.
func (h *Handler) ListProjectCrons(w http.ResponseWriter, r *http.Request) {
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
		h.log.Error("list project_crons: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows, err := h.store.ListProjectCrons(r.Context(), detail.Project.ID)
	if err != nil {
		h.log.Error("list project_crons", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]projectCronDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, toDTO(c))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(projectCronsResponse{Crons: out})
}

// CreateProjectCron handles POST /api/v1/projects/{slug}/crons.
func (h *Handler) CreateProjectCron(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	req, ok := readCronRequest(w, r)
	if !ok {
		return
	}
	pipelineIDs, err := parsePipelineIDs(req.PipelineIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("create project_cron: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var createdBy *uuid.UUID
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		id := u.ID
		createdBy = &id
	}
	created, err := h.store.InsertProjectCron(r.Context(), store.ProjectCronInput{
		ProjectID:   detail.Project.ID,
		Name:        req.Name,
		Expression:  req.Expression,
		PipelineIDs: pipelineIDs,
		Enabled:     req.Enabled,
		CreatedBy:   createdBy,
	})
	if err != nil {
		h.log.Error("create project_cron", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectCronCreate, "project_cron", created.ID.String(),
		map[string]any{"slug": slug, "name": created.Name, "expression": created.Expression})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toDTO(created))
}

// UpdateProjectCron handles PUT /api/v1/projects/{slug}/crons/{id}.
func (h *Handler) UpdateProjectCron(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid cron id", http.StatusBadRequest)
		return
	}
	req, ok := readCronRequest(w, r)
	if !ok {
		return
	}
	pipelineIDs, err := parsePipelineIDs(req.PipelineIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	existing, err := h.store.GetProjectCron(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrProjectCronNotFound) {
			http.Error(w, "cron not found", http.StatusNotFound)
			return
		}
		h.log.Error("update project_cron: load", "slug", slug, "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Resolve slug → project and match against the cron's project id
	// so a guessed slug can't escalate onto someone else's schedule.
	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("update project_cron: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if existing.ProjectID != detail.Project.ID {
		http.Error(w, "cron does not belong to this project", http.StatusNotFound)
		return
	}

	if err := h.store.UpdateProjectCron(r.Context(), id, store.ProjectCronInput{
		ProjectID:   existing.ProjectID,
		Name:        req.Name,
		Expression:  req.Expression,
		PipelineIDs: pipelineIDs,
		Enabled:     req.Enabled,
	}); err != nil {
		h.log.Error("update project_cron", "slug", slug, "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectCronUpdate, "project_cron", id.String(),
		map[string]any{"slug": slug, "name": req.Name, "expression": req.Expression,
			"enabled": req.Enabled})
	w.WriteHeader(http.StatusNoContent)
}

// DeleteProjectCron handles DELETE /api/v1/projects/{slug}/crons/{id}.
func (h *Handler) DeleteProjectCron(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid cron id", http.StatusBadRequest)
		return
	}
	// Slug-vs-project match so a delete can't cross tenants.
	existing, err := h.store.GetProjectCron(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrProjectCronNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.log.Error("delete project_cron: load", "slug", slug, "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil || existing.ProjectID != detail.Project.ID {
		http.Error(w, "cron not found", http.StatusNotFound)
		return
	}

	if err := h.store.DeleteProjectCron(r.Context(), id); err != nil {
		h.log.Error("delete project_cron", "slug", slug, "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectCronDelete, "project_cron", id.String(),
		map[string]any{"slug": slug, "name": existing.Name})
	w.WriteHeader(http.StatusNoContent)
}

// runAllResultDTO echoes cron.RunAllResult with string ids for the
// JSON wire.
type runAllResultDTO struct {
	PipelineID string `json:"pipeline_id"`
	RunID      string `json:"run_id,omitempty"`
	Error      string `json:"error,omitempty"`
}

type runAllResponse struct {
	Results []runAllResultDTO `json:"results"`
}

// RunAllPipelines handles POST /api/v1/projects/{slug}/run-all.
// Queues a manual run for every pipeline in the project; returns
// a list of (pipeline_id → run_id | error) so the UI surfaces
// per-pipeline outcomes without a page reload.
func (h *Handler) RunAllPipelines(w http.ResponseWriter, r *http.Request) {
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
		h.log.Error("run-all: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	triggeredBy := "manual"
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		if u.Email != "" {
			triggeredBy = u.Email
		}
	}
	results, err := cron.RunAll(r.Context(), h.store, detail.Project.ID, triggeredBy)
	if err != nil {
		h.log.Error("run-all", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := make([]runAllResultDTO, 0, len(results))
	fired := 0
	for _, r := range results {
		item := runAllResultDTO{PipelineID: r.PipelineID.String(), Error: r.Error}
		if r.RunID != nil {
			item.RunID = r.RunID.String()
			fired++
		}
		out = append(out, item)
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectRunAll, "project", slug,
		map[string]any{"slug": slug, "fired": fired, "total": len(results)})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(runAllResponse{Results: out})
}

func readCronRequest(w http.ResponseWriter, r *http.Request) (projectCronWriteRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxProjectCronBytes)
	var req projectCronWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return req, false
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Expression = strings.TrimSpace(req.Expression)
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return req, false
	}
	if req.Expression == "" {
		http.Error(w, "expression is required", http.StatusBadRequest)
		return req, false
	}
	if _, err := cronParser.Parse(req.Expression); err != nil {
		http.Error(w, "invalid cron expression: "+err.Error(), http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func parsePipelineIDs(in []string) ([]uuid.UUID, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]uuid.UUID, 0, len(in))
	for _, s := range in {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, errors.New("pipeline_ids contains invalid uuid: " + s)
		}
		out = append(out, id)
	}
	return out, nil
}

func toDTO(c store.ProjectCron) projectCronDTO {
	ids := make([]string, 0, len(c.PipelineIDs))
	for _, id := range c.PipelineIDs {
		ids = append(ids, id.String())
	}
	dto := projectCronDTO{
		ID:          c.ID.String(),
		ProjectID:   c.ProjectID.String(),
		Name:        c.Name,
		Expression:  c.Expression,
		PipelineIDs: ids,
		Enabled:     c.Enabled,
		LastFiredAt: c.LastFiredAt,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
	if c.CreatedBy != nil {
		dto.CreatedBy = c.CreatedBy.String()
	}
	return dto
}
