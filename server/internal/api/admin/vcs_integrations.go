package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/config"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
)

// VCSIntegrationsHandler backs /api/v1/admin/integrations/vcs/*.
// Keeps a reference to the live vcs.Registry so CRUD writes
// hot-reload without a restart — the checks reporter + auto-
// register consume the same registry via vcs.Registry.GitHubApp().
type VCSIntegrationsHandler struct {
	store    *store.Store
	registry *vcs.Registry
	cfg      *config.Config
	log      *slog.Logger
}

// NewVCSIntegrationsHandler wires the handler. All fields are
// required; nil is a programming error.
func NewVCSIntegrationsHandler(s *store.Store, registry *vcs.Registry, cfg *config.Config, log *slog.Logger) *VCSIntegrationsHandler {
	if log == nil {
		log = slog.Default()
	}
	return &VCSIntegrationsHandler{store: s, registry: registry, cfg: cfg, log: log}
}

// List handles GET /api/v1/admin/integrations/vcs. Returns the
// merged view: env-bootstrapped rows (read-only, source=env) +
// DB-managed rows (editable, source=db). The UI uses `source` to
// decide whether to render edit/delete buttons for each row.
func (h *VCSIntegrationsHandler) List(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	// Pull DB rows separately so the response carries BOTH the
	// admin-facing shape (with HasPrivateKey, HasWebhookSecret
	// flags) AND the registry's unified Source view. The two
	// overlap but serve different UI needs.
	dbRows, err := h.store.ListConfiguredVCSIntegrations(r.Context())
	if err != nil {
		h.log.Error("list vcs integrations", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	active := h.registry.List()
	writeJSON(w, map[string]any{
		"integrations": dbRows,
		"active":       active,
	})
}

// Upsert handles POST /api/v1/admin/integrations/vcs. Creates or
// updates a row by `name`, then reloads the registry so the new
// client takes effect immediately.
func (h *VCSIntegrationsHandler) Upsert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name          string `json:"name"`
		Kind          string `json:"kind"`
		DisplayName   string `json:"display_name"`
		AppID         *int64 `json:"app_id,omitempty"`
		PrivateKey    string `json:"private_key"`    // empty = preserve
		WebhookSecret string `json:"webhook_secret"` // empty = preserve
		APIBase       string `json:"api_base"`
		Enabled       bool   `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	created, err := h.store.UpsertVCSIntegration(r.Context(), store.UpsertVCSIntegrationInput{
		Name:          body.Name,
		Kind:          body.Kind,
		DisplayName:   body.DisplayName,
		AppID:         body.AppID,
		PrivateKeyPEM: []byte(body.PrivateKey),
		WebhookSecret: body.WebhookSecret,
		APIBase:       body.APIBase,
		Enabled:       body.Enabled,
	})
	if errors.Is(err, store.ErrAuthProviderCipherUnset) {
		http.Error(w, "GOCDNEXT_SECRET_KEY must be set to store VCS secrets", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		// Store validation errors are safe to pass back as 400.
		h.log.Warn("upsert vcs integration", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if reloadErr := h.reload(r.Context()); reloadErr != nil {
		h.log.Warn("reload after upsert", "err", reloadErr)
		writeJSONStatus(w, http.StatusCreated, map[string]any{
			"integration":    created,
			"reload_warning": reloadErr.Error(),
		})
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{"integration": created})
}

// Delete handles DELETE /api/v1/admin/integrations/vcs/{id}.
func (h *VCSIntegrationsHandler) Delete(w http.ResponseWriter, r *http.Request) {
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
	err = h.store.DeleteVCSIntegration(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrVCSIntegrationNotFound):
		http.Error(w, "vcs integration not found", http.StatusNotFound)
		return
	case err != nil:
		h.log.Error("delete vcs integration", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if reloadErr := h.reload(r.Context()); reloadErr != nil {
		h.log.Warn("reload after delete", "err", reloadErr)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Reload handles POST /api/v1/admin/integrations/vcs/reload. Rare
// escape hatch for when a DB row was edited out-of-band (dump
// restore, manual psql).
func (h *VCSIntegrationsHandler) Reload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := h.reload(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"active": h.registry.List()})
}

func (h *VCSIntegrationsHandler) reload(ctx context.Context) error {
	return vcs.Reload(ctx, h.registry, h.cfg, h.store, h.log)
}
