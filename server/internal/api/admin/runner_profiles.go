package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// runnerProfileDTO is the wire shape /api/v1/admin/runner-profiles
// returns. Strings carry the raw k8s-quantity format so the UI
// echoes them as-is — no conversion round-trip on read.
//
// Env carries plain key/value pairs the agent injects into every
// plugin container that runs on this profile. SecretKeys returns
// only the names — values stay encrypted at rest and never leave
// the server through this endpoint. The UI shows "•••" next to
// each key so admins know a secret is configured without exposing
// its value.
type runnerProfileDTO struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	Engine            string            `json:"engine"`
	DefaultImage      string            `json:"default_image"`
	DefaultCPURequest string            `json:"default_cpu_request"`
	DefaultCPULimit   string            `json:"default_cpu_limit"`
	DefaultMemRequest string            `json:"default_mem_request"`
	DefaultMemLimit   string            `json:"default_mem_limit"`
	MaxCPU            string            `json:"max_cpu"`
	MaxMem            string            `json:"max_mem"`
	Tags              []string          `json:"tags"`
	Config            map[string]any    `json:"config,omitempty"`
	Env               map[string]string `json:"env"`
	SecretKeys        []string          `json:"secret_keys"`
	// SecretRefs maps a secret KEY → the global secret NAME it
	// references via `{{secret:NAME}}`. Only populated for rows
	// where the value is a single template; mixed values
	// (template + literal text) and pure literals don't appear
	// here. UI uses this to render "→ globals.NAME" in place of
	// the masked-value placeholder.
	SecretRefs map[string]string `json:"secret_refs"`
	CreatedAt  string            `json:"created_at"`
	UpdatedAt  string            `json:"updated_at"`
}

type runnerProfilesResponse struct {
	Profiles []runnerProfileDTO `json:"profiles"`
}

// runnerProfileWriteRequest is the create/update payload.
//
// Env is full-replace (the new map replaces the old). Secrets is
// also full-replace BUT empty/missing values mean "remove this
// key" — there's no plaintext placeholder to keep an existing
// value, since the wire format already forces a decision on the
// client side. The UI must read SecretKeys, decide which to keep,
// and send back the full set; missing keys are deletions.
type runnerProfileWriteRequest struct {
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	Engine            string            `json:"engine"`
	DefaultImage      string            `json:"default_image"`
	DefaultCPURequest string            `json:"default_cpu_request"`
	DefaultCPULimit   string            `json:"default_cpu_limit"`
	DefaultMemRequest string            `json:"default_mem_request"`
	DefaultMemLimit   string            `json:"default_mem_limit"`
	MaxCPU            string            `json:"max_cpu"`
	MaxMem            string            `json:"max_mem"`
	Tags              []string          `json:"tags"`
	Config            map[string]any    `json:"config,omitempty"`
	Env               map[string]string `json:"env"`
	Secrets           map[string]string `json:"secrets"`
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
		// Pull the {{secret:NAME}} reference map per profile so the
		// editor can render "→ globals.NAME" chips. Errors here are
		// non-fatal: if decryption fails we just omit the refs map
		// — the literal-vs-ref distinction degrades to "all look
		// like literals", but the page still renders.
		refs, err := h.store.ProfileSecretRefs(r.Context(), h.cipher, p.ID)
		if err != nil {
			h.log.Warn("admin runner-profiles: read refs", "profile", p.Name, "err", err)
		}
		out = append(out, toRunnerProfileDTO(p, refs))
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
	if len(req.Secrets) > 0 && h.cipher == nil {
		http.Error(w, "secrets feature unavailable: server cipher not configured", http.StatusServiceUnavailable)
		return
	}
	p, err := h.store.InsertRunnerProfile(r.Context(), h.cipher, runnerProfileInputFromReq(req))
	if err != nil {
		if strings.Contains(err.Error(), "runner_profiles_name_key") {
			http.Error(w, "profile name already exists", http.StatusConflict)
			return
		}
		h.log.Error("admin runner-profiles: create", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Audit captures KEYS only — never values. Operators tracing
	// "who added the AWS key" need the key name + actor + time;
	// the value belongs in the encrypted column, period.
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionRunnerProfileCreate, "runner_profile", p.ID.String(),
		map[string]any{
			"name":        p.Name,
			"engine":      p.Engine,
			"env_keys":    sortedMapKeys(req.Env),
			"secret_keys": sortedMapKeys(req.Secrets),
		})
	// Re-read refs so the create response can populate them when
	// the new profile carries `{{secret:NAME}}` template values.
	// Read failure here is non-fatal — the row is already
	// persisted, refs just degrade to empty in the response.
	refs, refsErr := h.store.ProfileSecretRefs(r.Context(), h.cipher, p.ID)
	if refsErr != nil {
		h.log.Warn("admin runner-profiles: read refs after create", "profile", p.Name, "err", refsErr)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toRunnerProfileDTO(p, refs))
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
	if len(req.Secrets) > 0 && h.cipher == nil {
		http.Error(w, "secrets feature unavailable: server cipher not configured", http.StatusServiceUnavailable)
		return
	}
	if err := h.store.UpdateRunnerProfile(r.Context(), h.cipher, id, runnerProfileInputFromReq(req)); err != nil {
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
		map[string]any{
			"name":        req.Name,
			"env_keys":    sortedMapKeys(req.Env),
			"secret_keys": sortedMapKeys(req.Secrets),
		})
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
	usage, err := h.store.CountRunnerProfileUsage(r.Context(), existing.Name)
	if err != nil {
		h.log.Error("admin runner-profiles: count usage", "name", existing.Name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if usage.Pipelines > 0 || usage.ActiveRuns > 0 {
		http.Error(w, formatProfileUsageError(usage), http.StatusConflict)
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

// formatProfileUsageError builds the 409 message the admin sees
// when a delete is blocked. Always names both axes (pipelines +
// active runs) so the operator knows whether the fix is "rewire
// pipelines" (static), "wait for runs to drain" (dynamic), or both.
func formatProfileUsageError(u store.RunnerProfileUsage) string {
	switch {
	case u.Pipelines > 0 && u.ActiveRuns > 0:
		return fmt.Sprintf(
			"profile is referenced by %d pipeline(s) with %d active run(s) — rewire the pipelines and wait for the runs to drain before deleting",
			u.Pipelines, u.ActiveRuns)
	case u.Pipelines > 0:
		return fmt.Sprintf(
			"profile is referenced by %d pipeline(s) — remove the references before deleting",
			u.Pipelines)
	case u.ActiveRuns > 0:
		return fmt.Sprintf(
			"profile is still bound to %d active run(s) (queued or running) — wait for them to finish or cancel them before deleting",
			u.ActiveRuns)
	}
	return "profile is in use"
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
	for k := range req.Env {
		if !validEnvKey(k) {
			http.Error(w, fmt.Sprintf("env key %q invalid: use UPPER_SNAKE_CASE (letters, digits, underscores)", k), http.StatusBadRequest)
			return req, false
		}
	}
	for k := range req.Secrets {
		if !validEnvKey(k) {
			http.Error(w, fmt.Sprintf("secret key %q invalid: use UPPER_SNAKE_CASE (letters, digits, underscores)", k), http.StatusBadRequest)
			return req, false
		}
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
		Env:               req.Env,
		Secrets:           req.Secrets,
	}
}

func toRunnerProfileDTO(p store.RunnerProfile, refs map[string]string) runnerProfileDTO {
	env := p.Env
	if env == nil {
		// Always emit a JSON object — the UI iterates `Object.entries`
		// which would crash on null. Empty map is the consistent
		// "nothing configured" representation on read.
		env = map[string]string{}
	}
	keys := p.SecretKeys
	if keys == nil {
		keys = []string{}
	}
	if refs == nil {
		refs = map[string]string{}
	}
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
		Env:               env,
		SecretKeys:        keys,
		SecretRefs:        refs,
		CreatedAt:         p.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:         p.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// sortedMapKeys returns the keys of m in lexical order. Used by the
// audit emit path so two writes with the same key set emit the same
// stable serialization, easier to grep through history.
func sortedMapKeys(m map[string]string) []string {
	if len(m) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validEnvKey enforces the conventional shape for env var names:
// uppercase letter or underscore start, then uppercase letters /
// digits / underscores. Bash, Docker, Kubernetes all converge on
// this; rejecting at write time is cheaper than a confusing
// runtime failure inside the plugin container.
func validEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i, c := range k {
		switch {
		case c == '_':
		case c >= 'A' && c <= 'Z':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}
