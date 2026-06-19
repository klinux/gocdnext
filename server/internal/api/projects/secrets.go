package projects

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const maxSecretBytes = 64 << 10 // 64 KiB — generous cap for PEM keys etc.

// WithCipher hands the handler a shared *crypto.Cipher so secret endpoints
// can encrypt db secrets on write and decrypt on read. A nil cipher disables
// db secrets (external references still work if a backend is configured).
func (h *Handler) WithCipher(c *crypto.Cipher) *Handler {
	h.cipher = c
	return h
}

// WithSecretSources records the external secret backends configured on this
// server, so the UI can offer them and writes can be validated.
func (h *Handler) WithSecretSources(sources []string) *Handler {
	set := make(map[string]bool, len(sources))
	for _, s := range sources {
		set[s] = true
	}
	sorted := append([]string(nil), sources...)
	sort.Strings(sorted)
	h.secretSources = sorted
	h.secretSourceSet = set
	return h
}

type secretRefInput struct {
	Path string `json:"path"`
	Key  string `json:"key"`
}

type setSecretRequest struct {
	Name   string          `json:"name"`
	Value  string          `json:"value"`
	Source string          `json:"source"`
	Ref    *secretRefInput `json:"ref"`
}

type secretsListResponse struct {
	Secrets []store.Secret `json:"secrets"`
	Total   int64          `json:"total"`
	Limit   int32          `json:"limit"`
	Offset  int32          `json:"offset"`
	// Inherited is the set of global-scope secrets that apply to this
	// project unless shadowed by a local name (value-free, unpaginated).
	Inherited         []store.Secret `json:"inherited,omitempty"`
	ConfiguredSources []string       `json:"configured_sources"`
}

// SetSecret handles POST /api/v1/projects/{slug}/secrets — a db value
// ({name,value}) or an external reference ({name,source,ref:{path,key}}).
func (h *Handler) SetSecret(w http.ResponseWriter, r *http.Request) {
	if !h.ensureSecretsConfigured(w) {
		return
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSecretBytes)
	var req setSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w,
				fmt.Sprintf("secret value too large — cap is %d KiB", maxSecretBytes>>10),
				http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := store.ValidateSecretName(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	source, refPath, refKey := normalizeProjectSecretWrite(req)
	if err := store.ValidateSecretRef(source, refPath, refKey, h.secretSourceSet); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if source == store.SecretSourceDB && h.cipher == nil {
		http.Error(w, "secrets disabled: GOCDNEXT_SECRET_KEY must be set for db-stored secrets", http.StatusServiceUnavailable)
		return
	}

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("set secret: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	created, err := h.store.SetSecret(r.Context(), h.cipher, store.SecretSet{
		ProjectID: detail.Project.ID,
		Name:      req.Name,
		Source:    source,
		Value:     []byte(req.Value),
		RefPath:   refPath,
		RefKey:    refKey,
	})
	if err != nil {
		h.log.Error("set secret", "slug", slug, "name", req.Name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.log.Info("secret set", "slug", slug, "name", req.Name, "source", source, "created", created)
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionSecretSet, "project_secret", slug+"/"+req.Name,
		map[string]any{"slug": slug, "name": req.Name, "source": source, "created": created})

	w.Header().Set("Content-Type", "application/json")
	if created {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"name": req.Name, "created": created})
}

// ListSecrets handles GET /api/v1/projects/{slug}/secrets — paginated,
// value-free, with the inherited globals (unpaginated) and configured sources.
func (h *Handler) ListSecrets(w http.ResponseWriter, r *http.Request) {
	if !h.ensureSecretsConfigured(w) {
		return
	}
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
		h.log.Error("list secrets", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	limit, offset := secretsLimitOffset(r)
	page, err := h.store.ListSecretsPaged(r.Context(), detail.Project.ID, limit, offset)
	if err != nil {
		h.log.Error("list secrets: store", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Inherited globals (filtered to names this project hasn't shadowed)
	// stay unpaginated — it's a bounded, filtered subset; the admin page
	// paginates the global list itself. The shadow set is built from ALL of
	// the project's local secret names, not just the current page — otherwise
	// a local secret on page 2 would still surface its global twin as
	// "inherited" on page 1, which is a lie (it's shadowed everywhere).
	globals, err := h.store.ListGlobalSecrets(r.Context())
	if err != nil {
		h.log.Error("list global secrets: store", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	allLocal, err := h.store.ListSecrets(r.Context(), detail.Project.ID)
	if err != nil {
		h.log.Error("list secrets: shadow set", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	local := make(map[string]struct{}, len(allLocal))
	for _, s := range allLocal {
		local[s.Name] = struct{}{}
	}
	inherited := make([]store.Secret, 0, len(globals))
	for _, g := range globals {
		if _, shadowed := local[g.Name]; shadowed {
			continue
		}
		inherited = append(inherited, g)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(secretsListResponse{
		Secrets:           page.Secrets,
		Total:             page.Total,
		Limit:             page.Limit,
		Offset:            page.Offset,
		Inherited:         inherited,
		ConfiguredSources: h.writableSources(),
	})
}

// writableSources is the set of sources a write may pick on THIS server, so
// the UI never offers one the server will reject: db only when a cipher is
// configured, plus every enabled external backend.
func (h *Handler) writableSources() []string {
	out := make([]string, 0, len(h.secretSources)+1)
	if h.cipher != nil {
		out = append(out, store.SecretSourceDB)
	}
	return append(out, h.secretSources...)
}

// DeleteSecret handles DELETE /api/v1/projects/{slug}/secrets/{name}.
func (h *Handler) DeleteSecret(w http.ResponseWriter, r *http.Request) {
	if !h.ensureSecretsConfigured(w) {
		return
	}
	slug := chi.URLParam(r, "slug")
	name := chi.URLParam(r, "name")
	if slug == "" || name == "" {
		http.Error(w, "slug and name are required", http.StatusBadRequest)
		return
	}

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("delete secret: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.store.DeleteSecret(r.Context(), detail.Project.ID, name); err != nil {
		if errors.Is(err, store.ErrSecretNotFound) {
			http.Error(w, "secret not found", http.StatusNotFound)
			return
		}
		h.log.Error("delete secret", "slug", slug, "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.log.Info("secret deleted", "slug", slug, "name", name)
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionSecretDelete, "project_secret", slug+"/"+name,
		map[string]any{"slug": slug, "name": name})

	w.WriteHeader(http.StatusNoContent)
}

// ensureSecretsConfigured allows the endpoint when the server can store a db
// secret (cipher set) OR resolve an external one (a backend configured).
func (h *Handler) ensureSecretsConfigured(w http.ResponseWriter) bool {
	if h.cipher == nil && len(h.secretSources) == 0 {
		http.Error(w, "secrets subsystem not configured (set GOCDNEXT_SECRET_KEY or enable an external backend)", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// normalizeProjectSecretWrite trims source/path/key (a stray space from a
// copy-paste would otherwise become a different reference + cache key that's
// hard to spot) and defaults an empty source to db.
func normalizeProjectSecretWrite(req setSecretRequest) (string, string, string) {
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = store.SecretSourceDB
	}
	if req.Ref == nil {
		return source, "", ""
	}
	return source, strings.TrimSpace(req.Ref.Path), strings.TrimSpace(req.Ref.Key)
}

// secretsLimitOffset reads ?limit (default 50, 1..200) + ?offset (>=0).
func secretsLimitOffset(r *http.Request) (int32, int32) {
	limit := int32(50)
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		if v > 200 {
			v = 200
		}
		limit = int32(v)
	}
	offset := int32(0)
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = int32(v)
	}
	return limit, offset
}
