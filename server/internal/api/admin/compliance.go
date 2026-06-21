package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/compliance"
)

// ---- DTOs ----------------------------------------------------------------

type frameworkDTO struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func toFrameworkDTO(f store.ComplianceFramework) frameworkDTO {
	return frameworkDTO{
		ID: f.ID, Name: f.Name, Description: f.Description,
		CreatedBy: f.CreatedBy, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
	}
}

type policyDTO struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Enabled        bool      `json:"enabled"`
	Mode           string    `json:"mode"`
	Priority       int       `json:"priority"`
	AppliesToAll   bool      `json:"applies_to_all"`
	PositionBefore string    `json:"position_before"`
	PositionAfter  string    `json:"position_after"`
	FrameworkIDs   []string  `json:"framework_ids"`
	ConfigYAML     string    `json:"config_yaml"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func toPolicyDTO(p store.CompliancePolicy) policyDTO {
	fw := p.FrameworkIDs
	if fw == nil {
		fw = []string{}
	}
	return policyDTO{
		ID: p.ID, Name: p.Name, Description: p.Description, Enabled: p.Enabled,
		Mode: p.Mode, Priority: p.Priority, AppliesToAll: p.AppliesToAll,
		PositionBefore: p.PositionBefore, PositionAfter: p.PositionAfter,
		FrameworkIDs: fw, ConfigYAML: p.ConfigYAML, CreatedBy: p.CreatedBy,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

type frameworkWriteRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type policyWriteRequest struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Enabled        bool     `json:"enabled"`
	Mode           string   `json:"mode"`
	Priority       int      `json:"priority"`
	AppliesToAll   bool     `json:"applies_to_all"`
	PositionBefore string   `json:"position_before"`
	PositionAfter  string   `json:"position_after"`
	FrameworkIDs   []string `json:"framework_ids"`
	ConfigYAML     string   `json:"config_yaml"`
}

type projectFrameworksRequest struct {
	FrameworkIDs []string `json:"framework_ids"`
}

// ---- frameworks ----------------------------------------------------------

// ComplianceFrameworks handles GET /api/v1/admin/compliance/frameworks.
func (h *Handler) ComplianceFrameworks(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	rows, err := h.store.ListComplianceFrameworks(r.Context())
	if err != nil {
		h.log.Error("admin compliance: list frameworks", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]frameworkDTO, 0, len(rows))
	for _, f := range rows {
		out = append(out, toFrameworkDTO(f))
	}
	writeJSON(w, out)
}

// CreateComplianceFramework handles POST /api/v1/admin/compliance/frameworks.
func (h *Handler) CreateComplianceFramework(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSON[frameworkWriteRequest](w, r)
	if !ok {
		return
	}
	in := store.FrameworkInput{Name: req.Name, Description: req.Description}
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		in.CreatedBy = u.Email
	}
	f, err := h.store.InsertComplianceFramework(r.Context(), in)
	if err != nil {
		h.writeComplianceErr(w, "create framework", err)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionComplianceFrameworkCreate, "compliance_framework", f.ID,
		map[string]any{"name": f.Name})
	writeJSONStatus(w, http.StatusCreated, toFrameworkDTO(f))
}

// UpdateComplianceFramework handles PUT /api/v1/admin/compliance/frameworks/{id}.
func (h *Handler) UpdateComplianceFramework(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	req, ok := decodeJSON[frameworkWriteRequest](w, r)
	if !ok {
		return
	}
	err := h.store.UpdateComplianceFramework(r.Context(), id, store.FrameworkInput{
		Name: req.Name, Description: req.Description,
	})
	if err != nil {
		h.writeComplianceErr(w, "update framework", err)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionComplianceFrameworkUpdate, "compliance_framework", id,
		map[string]any{"name": req.Name})
	w.WriteHeader(http.StatusNoContent)
}

// DeleteComplianceFramework handles DELETE /api/v1/admin/compliance/frameworks/{id}.
// Blocked (409) while the framework is still assigned to a project or targeted
// by a policy — the operator must unassign/retarget first.
func (h *Handler) DeleteComplianceFramework(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	usage, err := h.store.FrameworkUsage(r.Context(), id)
	if err != nil {
		h.writeComplianceErr(w, "framework usage", err)
		return
	}
	if usage.Projects > 0 || usage.Policies > 0 {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error":    "framework in use",
			"projects": usage.Projects,
			"policies": usage.Policies,
		})
		return
	}
	if err := h.store.DeleteComplianceFramework(r.Context(), id); err != nil {
		h.writeComplianceErr(w, "delete framework", err)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionComplianceFrameworkDelete, "compliance_framework", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- project ↔ framework assignment --------------------------------------

// ProjectFrameworks handles GET /api/v1/admin/projects/{slug}/frameworks.
func (h *Handler) ProjectFrameworks(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	pid, err := h.store.ProjectIDBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		h.writeComplianceErr(w, "project frameworks", err)
		return
	}
	rows, err := h.store.ListProjectFrameworks(r.Context(), pid)
	if err != nil {
		h.log.Error("admin compliance: project frameworks", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]frameworkDTO, 0, len(rows))
	for _, f := range rows {
		out = append(out, toFrameworkDTO(f))
	}
	writeJSON(w, out)
}

// SetProjectFrameworks handles PUT /api/v1/admin/projects/{slug}/frameworks.
func (h *Handler) SetProjectFrameworks(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	req, ok := decodeJSON[projectFrameworksRequest](w, r)
	if !ok {
		return
	}
	pid, err := h.store.ProjectIDBySlug(r.Context(), slug)
	if err != nil {
		h.writeComplianceErr(w, "set project frameworks", err)
		return
	}
	if err := h.store.SetProjectFrameworks(r.Context(), pid, req.FrameworkIDs); err != nil {
		h.writeComplianceErr(w, "set project frameworks", err)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectFrameworksSet, "project", slug,
		map[string]any{"framework_ids": req.FrameworkIDs})
	w.WriteHeader(http.StatusNoContent)
}

// ---- policies ------------------------------------------------------------

// CompliancePolicies handles GET /api/v1/admin/compliance/policies.
func (h *Handler) CompliancePolicies(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	rows, err := h.store.ListCompliancePolicies(r.Context())
	if err != nil {
		h.log.Error("admin compliance: list policies", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]policyDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, toPolicyDTO(p))
	}
	writeJSON(w, out)
}

// CompliancePolicy handles GET /api/v1/admin/compliance/policies/{id}.
func (h *Handler) CompliancePolicy(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	p, err := h.store.GetCompliancePolicy(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeComplianceErr(w, "get policy", err)
		return
	}
	writeJSON(w, toPolicyDTO(p))
}

// CreateCompliancePolicy handles POST /api/v1/admin/compliance/policies.
func (h *Handler) CreateCompliancePolicy(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSON[policyWriteRequest](w, r)
	if !ok {
		return
	}
	if !h.validatePolicyConfig(w, req) {
		return
	}
	in := policyInputFromReq(req)
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		in.CreatedBy = u.Email
	}
	p, err := h.store.InsertCompliancePolicy(r.Context(), in)
	if err != nil {
		h.writeComplianceErr(w, "create policy", err)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionCompliancePolicyCreate, "compliance_policy", p.ID,
		map[string]any{"name": p.Name, "mode": p.Mode, "applies_to_all": p.AppliesToAll})
	writeJSONStatus(w, http.StatusCreated, toPolicyDTO(p))
}

// UpdateCompliancePolicy handles PUT /api/v1/admin/compliance/policies/{id}.
func (h *Handler) UpdateCompliancePolicy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	req, ok := decodeJSON[policyWriteRequest](w, r)
	if !ok {
		return
	}
	if !h.validatePolicyConfig(w, req) {
		return
	}
	if err := h.store.UpdateCompliancePolicy(r.Context(), id, policyInputFromReq(req)); err != nil {
		h.writeComplianceErr(w, "update policy", err)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionCompliancePolicyUpdate, "compliance_policy", id,
		map[string]any{"name": req.Name, "mode": req.Mode})
	w.WriteHeader(http.StatusNoContent)
}

// DeleteCompliancePolicy handles DELETE /api/v1/admin/compliance/policies/{id}.
func (h *Handler) DeleteCompliancePolicy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteCompliancePolicy(r.Context(), id); err != nil {
		h.writeComplianceErr(w, "delete policy", err)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionCompliancePolicyDelete, "compliance_policy", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers -------------------------------------------------------------

func policyInputFromReq(req policyWriteRequest) store.PolicyInput {
	return store.PolicyInput{
		Name: req.Name, Description: req.Description, Enabled: req.Enabled,
		Mode: req.Mode, Priority: req.Priority, AppliesToAll: req.AppliesToAll,
		PositionBefore: req.PositionBefore, PositionAfter: req.PositionAfter,
		FrameworkIDs: req.FrameworkIDs, ConfigYAML: req.ConfigYAML,
	}
}

// validatePolicyConfig surfaces author errors (bad mode, conflicting position,
// invalid/non-prefixed config YAML) as a clean 400 before hitting the store.
func (h *Handler) validatePolicyConfig(w http.ResponseWriter, req policyWriteRequest) bool {
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return false
	}
	if req.Mode != "" && req.Mode != compliance.ModeInject && req.Mode != compliance.ModeOverride {
		http.Error(w, "mode must be inject or override", http.StatusBadRequest)
		return false
	}
	if req.PositionBefore != "" && req.PositionAfter != "" {
		http.Error(w, "position_before and position_after are mutually exclusive", http.StatusBadRequest)
		return false
	}
	if _, err := compliance.CompilePolicy(req.ConfigYAML); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// writeComplianceErr maps store sentinels to HTTP statuses.
func (h *Handler) writeComplianceErr(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, store.ErrFrameworkNotFound),
		errors.Is(err, store.ErrPolicyNotFound),
		errors.Is(err, store.ErrProjectNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrFrameworkInUse):
		http.Error(w, "framework in use", http.StatusConflict)
	case errors.Is(err, store.ErrComplianceWouldDropEnforcement):
		// e.g. assigning a framework to a project with no SCM binding, or a
		// global policy that would govern such a project.
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		h.log.Error("admin compliance: "+op, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// decodeJSON is a small typed body decoder: 400 on malformed JSON.
func decodeJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return v, false
	}
	return v, true
}
