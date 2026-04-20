package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/auth"
	"github.com/gocdnext/gocdnext/server/internal/config"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// AuthProvidersHandler owns the /api/v1/admin/auth/providers
// endpoints. It keeps a reference to the live Registry so writes
// can hot-reload without a restart. Kept in its own file so the
// base admin Handler struct doesn't grow unmanageably.
type AuthProvidersHandler struct {
	store    *store.Store
	registry *auth.Registry
	cfg      *config.Config
	log      *slog.Logger
}

// NewAuthProvidersHandler wires the handler. All four pointers are
// required; nil inputs are a programming error (panic via use).
func NewAuthProvidersHandler(s *store.Store, registry *auth.Registry, cfg *config.Config, log *slog.Logger) *AuthProvidersHandler {
	if log == nil {
		log = slog.Default()
	}
	return &AuthProvidersHandler{store: s, registry: registry, cfg: cfg, log: log}
}

// List handles GET /api/v1/admin/auth/providers. Returns a JSON
// envelope with configured DB rows (secrets masked) + the set of
// currently active provider names from the in-memory registry so
// the UI can show "env-only" entries distinct from DB-managed ones.
func (h *AuthProvidersHandler) List(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	configured, err := h.store.ListConfiguredProviders(r.Context())
	if err != nil {
		h.log.Error("list auth providers", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Cross-reference in-memory registry: a provider active at
	// runtime that has no DB row must be env-only, which is useful
	// context for an admin deciding whether to migrate it.
	active := make(map[string]bool, h.registry.Len())
	for _, p := range h.registry.List() {
		active[string(p.Name())] = true
	}
	configuredNames := make(map[string]bool, len(configured))
	for _, c := range configured {
		configuredNames[c.Name] = true
	}
	envOnly := make([]string, 0)
	for name := range active {
		if !configuredNames[name] {
			envOnly = append(envOnly, name)
		}
	}

	writeJSON(w, map[string]any{
		"enabled":   h.cfg.AuthEnabled,
		"providers": configured,
		"env_only":  envOnly,
	})
}

// Upsert handles POST /api/v1/admin/auth/providers. Creates or
// updates a row by `name`, then rebuilds the in-memory registry so
// the new config takes effect immediately.
func (h *AuthProvidersHandler) Upsert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name          string `json:"name"`
		Kind          string `json:"kind"`
		DisplayName   string `json:"display_name"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		Issuer        string `json:"issuer"`
		GitHubAPIBase string `json:"github_api_base"`
		Enabled       bool   `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	created, err := h.store.UpsertConfiguredProvider(r.Context(), store.UpsertAuthProviderInput{
		Name:          body.Name,
		Kind:          body.Kind,
		DisplayName:   body.DisplayName,
		ClientID:      body.ClientID,
		ClientSecret:  body.ClientSecret,
		Issuer:        body.Issuer,
		GitHubAPIBase: body.GitHubAPIBase,
		Enabled:       body.Enabled,
	})
	if errors.Is(err, store.ErrAuthProviderCipherUnset) {
		http.Error(w, "GOCDNEXT_SECRET_KEY must be set to store auth provider secrets", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		// The validation errors from the store are safe to pass
		// back as 400 — they don't leak internals.
		h.log.Warn("upsert auth provider", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if reloadErr := h.reload(r.Context()); reloadErr != nil {
		// Row is persisted — the error is just the in-memory
		// rebuild. Return 201 with a warning field so the UI can
		// surface it alongside success.
		h.log.Warn("reload after upsert", "err", reloadErr)
		writeJSONStatus(w, http.StatusCreated, map[string]any{
			"provider":       created,
			"reload_warning": reloadErr.Error(),
		})
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{"provider": created})
}

// Delete handles DELETE /api/v1/admin/auth/providers/{id}.
func (h *AuthProvidersHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := chi.URLParam(r, "id")
	id, err := uuid.Parse(raw)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	err = h.store.DeleteConfiguredProvider(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrAuthProviderNotFound):
		http.Error(w, "auth provider not found", http.StatusNotFound)
		return
	case err != nil:
		h.log.Error("delete auth provider", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if reloadErr := h.reload(r.Context()); reloadErr != nil {
		h.log.Warn("reload after delete", "err", reloadErr)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Reload handles POST /api/v1/admin/auth/providers/reload. Useful
// when an admin edits env vars + a re-sync from outside gocdnext
// (DB dump restore, manual row edit). Returns 200 with the list of
// active names so the UI can reconcile its local state.
func (h *AuthProvidersHandler) Reload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := h.reload(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := make([]string, 0, h.registry.Len())
	for _, p := range h.registry.List() {
		names = append(names, string(p.Name()))
	}
	writeJSON(w, map[string]any{"active": names})
}

func (h *AuthProvidersHandler) reload(ctx context.Context) error {
	return auth.Reload(ctx, h.registry, h.cfg, h.store, h.log)
}

// writeJSONStatus is the same as writeJSON but with a caller-picked
// status code. Kept small to match the existing admin package style.
func writeJSONStatus(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
