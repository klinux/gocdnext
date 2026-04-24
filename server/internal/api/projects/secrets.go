package projects

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const maxSecretBytes = 64 << 10 // 64 KiB — generous cap for PEM keys etc.

// WithCipher hands the handler a shared *crypto.Cipher so secret endpoints
// can encrypt on write and decrypt on read. A nil cipher disables the
// endpoints (503).
func (h *Handler) WithCipher(c *crypto.Cipher) *Handler {
	h.cipher = c
	return h
}

type setSecretRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type secretsListResponse struct {
	Secrets []store.Secret `json:"secrets"`
	// Inherited is the set of global-scope secrets that apply to
	// this project at resolution time unless the same name is
	// defined locally. Surface names + timestamps so the UI can
	// show "X comes from global scope" without leaking values;
	// admins manage these on /settings/secrets.
	Inherited []store.Secret `json:"inherited,omitempty"`
}

// SetSecret handles POST /api/v1/projects/{slug}/secrets.
// Body: { "name": "FOO", "value": "ghp_..." }.
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
		Value:     []byte(req.Value),
	})
	if err != nil {
		h.log.Error("set secret", "slug", slug, "name", req.Name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.log.Info("secret set", "slug", slug, "name", req.Name, "created", created)
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionSecretSet, "project_secret", slug+"/"+req.Name,
		map[string]any{"slug": slug, "name": req.Name, "created": created})

	w.Header().Set("Content-Type", "application/json")
	if created {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":    req.Name,
		"created": created,
	})
}

// ListSecrets handles GET /api/v1/projects/{slug}/secrets.
// Never returns values — only names + timestamps.
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

	secrets, err := h.store.ListSecrets(r.Context(), detail.Project.ID)
	if err != nil {
		h.log.Error("list secrets: store", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Filter globals down to names the project hasn't shadowed —
	// same rule the resolver applies at runtime. A project-scoped
	// "FOO" hides the global "FOO"; no point showing it as
	// inherited when the project one already wins.
	globals, err := h.store.ListGlobalSecrets(r.Context())
	if err != nil {
		h.log.Error("list global secrets: store", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	local := make(map[string]struct{}, len(secrets))
	for _, s := range secrets {
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
		Secrets:   secrets,
		Inherited: inherited,
	})
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

func (h *Handler) ensureSecretsConfigured(w http.ResponseWriter) bool {
	if h.cipher == nil {
		http.Error(w, "secrets subsystem not configured (set GOCDNEXT_SECRET_KEY)", http.StatusServiceUnavailable)
		return false
	}
	return true
}

