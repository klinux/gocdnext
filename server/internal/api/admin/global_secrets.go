package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const maxGlobalSecretBytes = 64 << 10 // 64 KiB — same cap as project secrets.

// GlobalSecretsHandler owns /api/v1/admin/secrets. Gated on role=admin
// by the router; the handler itself assumes the caller is authorized.
//
// Kept separate from Handler (the read-only admin handler) so the
// cipher dependency stays scoped: other admin endpoints don't need
// encryption and shouldn't carry the field around.
type GlobalSecretsHandler struct {
	store  *store.Store
	cipher *crypto.Cipher
	log    *slog.Logger
}

func NewGlobalSecretsHandler(s *store.Store, cipher *crypto.Cipher, log *slog.Logger) *GlobalSecretsHandler {
	if log == nil {
		log = slog.Default()
	}
	return &GlobalSecretsHandler{store: s, cipher: cipher, log: log}
}

type setGlobalSecretRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type globalSecretsListResponse struct {
	Secrets []store.Secret `json:"secrets"`
}

// Set handles POST /api/v1/admin/secrets. Creates or overwrites a
// global secret. Returns 201 Created on insert, 200 on update.
// 503 when the server has no cipher (GOCDNEXT_SECRET_KEY unset).
func (h *GlobalSecretsHandler) Set(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConfigured(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxGlobalSecretBytes)
	var req setGlobalSecretRequest
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

	created, err := h.store.SetGlobalSecret(r.Context(), h.cipher, req.Name, []byte(req.Value))
	if err != nil {
		h.log.Error("set global secret", "name", req.Name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.log.Info("global secret set", "name", req.Name, "created", created)
	w.Header().Set("Content-Type", "application/json")
	if created {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":    req.Name,
		"created": created,
	})
}

// List handles GET /api/v1/admin/secrets. Returns names + timestamps
// for every global secret. Values never cross the wire — the runtime
// resolver is the only reader.
func (h *GlobalSecretsHandler) List(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConfigured(w) {
		return
	}
	secrets, err := h.store.ListGlobalSecrets(r.Context())
	if err != nil {
		h.log.Error("list global secrets", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(globalSecretsListResponse{Secrets: secrets})
}

// Delete handles DELETE /api/v1/admin/secrets/{name}. 404 when the
// name doesn't match any global row.
func (h *GlobalSecretsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConfigured(w) {
		return
	}
	name := chi.URLParam(r, "name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	err := h.store.DeleteGlobalSecret(r.Context(), name)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrSecretNotFound):
		http.Error(w, "secret not found", http.StatusNotFound)
	default:
		h.log.Error("delete global secret", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// ensureConfigured short-circuits with 503 when the cipher isn't
// wired. Mirrors the project secrets handler's gate so operators
// see the same error regardless of scope.
func (h *GlobalSecretsHandler) ensureConfigured(w http.ResponseWriter) bool {
	if h.cipher == nil {
		http.Error(w,
			"secrets disabled: GOCDNEXT_SECRET_KEY must be set",
			http.StatusServiceUnavailable)
		return false
	}
	return true
}
