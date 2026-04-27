package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// runnerProfileDTO is the wire shape /api/v1/admin/runner-profiles
// returns. Strings carry the raw k8s-quantity format so the UI
// echoes them as-is — no conversion round-trip on read.
type runnerProfileDTO struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	Description       string         `json:"description"`
	Engine            string         `json:"engine"`
	DefaultImage      string         `json:"default_image"`
	DefaultCPURequest string         `json:"default_cpu_request"`
	DefaultCPULimit   string         `json:"default_cpu_limit"`
	DefaultMemRequest string         `json:"default_mem_request"`
	DefaultMemLimit   string         `json:"default_mem_limit"`
	MaxCPU            string         `json:"max_cpu"`
	MaxMem            string         `json:"max_mem"`
	Tags              []string       `json:"tags"`
	Config            map[string]any `json:"config,omitempty"`
	CreatedAt         string         `json:"created_at"`
	UpdatedAt         string         `json:"updated_at"`
}

type runnerProfilesResponse struct {
	Profiles []runnerProfileDTO `json:"profiles"`
}

type runnerProfileWriteRequest struct {
	Name              string         `json:"name"`
	Description       string         `json:"description"`
	Engine            string         `json:"engine"`
	DefaultImage      string         `json:"default_image"`
	DefaultCPURequest string         `json:"default_cpu_request"`
	DefaultCPULimit   string         `json:"default_cpu_limit"`
	DefaultMemRequest string         `json:"default_mem_request"`
	DefaultMemLimit   string         `json:"default_mem_limit"`
	MaxCPU            string         `json:"max_cpu"`
	MaxMem            string         `json:"max_mem"`
	Tags              []string       `json:"tags"`
	Config            map[string]any `json:"config,omitempty"`
}

// supportedEngines is the allow-list checked at write time. Mirrors
// the DB CHECK constraint so a typo surfaces as a 400 with a
// readable message instead of a Postgres error in the response.
var supportedEngines = map[string]struct{}{
	"kubernetes": {},
}

// RunnerProfiles handles GET /api/v1/admin/runner-profiles.
func (h *Handler) RunnerProfiles(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	rows, err := h.store.ListRunnerProfiles(r.Context())
	if err != nil {
		h.log.Error("admin runner-profiles: list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]runnerProfileDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, toRunnerProfileDTO(p))
	}
	writeJSON(w, runnerProfilesResponse{Profiles: out})
}

// CreateRunnerProfile handles POST /api/v1/admin/runner-profiles.
func (h *Handler) CreateRunnerProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, ok := decodeRunnerProfileWrite(w, r)
	if !ok {
		return
	}
	p, err := h.store.InsertRunnerProfile(r.Context(), runnerProfileInputFromReq(req))
	if err != nil {
		if strings.Contains(err.Error(), "runner_profiles_name_key") {
			http.Error(w, "profile name already exists", http.StatusConflict)
			return
		}
		h.log.Error("admin runner-profiles: create", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionRunnerProfileCreate, "runner_profile", p.ID.String(),
		map[string]any{"name": p.Name, "engine": p.Engine})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toRunnerProfileDTO(p))
}

// UpdateRunnerProfile handles PUT /api/v1/admin/runner-profiles/{id}.
func (h *Handler) UpdateRunnerProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := parseRunnerProfileID(w, r)
	if !ok {
		return
	}
	req, ok := decodeRunnerProfileWrite(w, r)
	if !ok {
		return
	}
	if err := h.store.UpdateRunnerProfile(r.Context(), id, runnerProfileInputFromReq(req)); err != nil {
		if errors.Is(err, store.ErrRunnerProfileNotFound) {
			http.Error(w, "profile not found", http.StatusNotFound)
			return
		}
		if strings.Contains(err.Error(), "runner_profiles_name_key") {
			http.Error(w, "profile name already exists", http.StatusConflict)
			return
		}
		h.log.Error("admin runner-profiles: update", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionRunnerProfileUpdate, "runner_profile", id.String(),
		map[string]any{"name": req.Name})
	w.WriteHeader(http.StatusNoContent)
}

// DeleteRunnerProfile handles DELETE /api/v1/admin/runner-profiles/{id}.
// Refuses to delete a profile that any pipeline definition still
// references (the resolver would 422 every apply afterwards). The
// admin must rename / unwire the pipelines first.
func (h *Handler) DeleteRunnerProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := parseRunnerProfileID(w, r)
	if !ok {
		return
	}
	existing, err := h.store.GetRunnerProfile(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrRunnerProfileNotFound) {
			http.Error(w, "profile not found", http.StatusNotFound)
			return
		}
		h.log.Error("admin runner-profiles: lookup before delete", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	count, err := h.store.CountPipelinesUsingRunnerProfile(r.Context(), existing.Name)
	if err != nil {
		h.log.Error("admin runner-profiles: count usage", "name", existing.Name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if count > 0 {
		http.Error(w, "profile is referenced by one or more pipelines — remove the references before deleting", http.StatusConflict)
		return
	}
	if err := h.store.DeleteRunnerProfile(r.Context(), id); err != nil {
		h.log.Error("admin runner-profiles: delete", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionRunnerProfileDelete, "runner_profile", id.String(),
		map[string]any{"name": existing.Name})
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func parseRunnerProfileID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid profile id", http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}

func decodeRunnerProfileWrite(w http.ResponseWriter, r *http.Request) (runnerProfileWriteRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var req runnerProfileWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return req, false
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return req, false
	}
	for _, c := range req.Name {
		if !(c == '-' || c == '_' || c == '.' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9')) {
			http.Error(w, "name may only contain letters, digits, dash, underscore, dot", http.StatusBadRequest)
			return req, false
		}
	}
	if req.Engine == "" {
		http.Error(w, "engine is required", http.StatusBadRequest)
		return req, false
	}
	if _, ok := supportedEngines[req.Engine]; !ok {
		http.Error(w, "unsupported engine (allowed: kubernetes)", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func runnerProfileInputFromReq(req runnerProfileWriteRequest) store.RunnerProfileInput {
	return store.RunnerProfileInput{
		Name:              req.Name,
		Description:       req.Description,
		Engine:            req.Engine,
		DefaultImage:      req.DefaultImage,
		DefaultCPURequest: req.DefaultCPURequest,
		DefaultCPULimit:   req.DefaultCPULimit,
		DefaultMemRequest: req.DefaultMemRequest,
		DefaultMemLimit:   req.DefaultMemLimit,
		MaxCPU:            req.MaxCPU,
		MaxMem:            req.MaxMem,
		Tags:              req.Tags,
		Config:            req.Config,
	}
}

func toRunnerProfileDTO(p store.RunnerProfile) runnerProfileDTO {
	return runnerProfileDTO{
		ID:                p.ID.String(),
		Name:              p.Name,
		Description:       p.Description,
		Engine:            p.Engine,
		DefaultImage:      p.DefaultImage,
		DefaultCPURequest: p.DefaultCPURequest,
		DefaultCPULimit:   p.DefaultCPULimit,
		DefaultMemRequest: p.DefaultMemRequest,
		DefaultMemLimit:   p.DefaultMemLimit,
		MaxCPU:            p.MaxCPU,
		MaxMem:            p.MaxMem,
		Tags:              p.Tags,
		Config:            p.Config,
		CreatedAt:         p.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:         p.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}
