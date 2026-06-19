package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const maxGlobalSecretBytes = 64 << 10 // 64 KiB — same cap as project secrets.

// GlobalSecretsHandler owns /api/v1/admin/secrets. Gated on role=admin by the
// router. sourcesFn reports the external secret backends enabled on this
// server, live (hot-reloaded) — for the source dropdown + write validation; a
// db secret needs the cipher, an external reference needs its backend enabled.
type GlobalSecretsHandler struct {
	store     *store.Store
	cipher    *crypto.Cipher
	sourcesFn func() []string
	log       *slog.Logger
}

func NewGlobalSecretsHandler(s *store.Store, cipher *crypto.Cipher, sourcesFn func() []string, log *slog.Logger) *GlobalSecretsHandler {
	if log == nil {
		log = slog.Default()
	}
	if sourcesFn == nil {
		sourcesFn = func() []string { return nil }
	}
	return &GlobalSecretsHandler{store: s, cipher: cipher, sourcesFn: sourcesFn, log: log}
}

// configuredSources is the live enabled external-backend set.
func (h *GlobalSecretsHandler) configuredSources() []string { return h.sourcesFn() }

// sourceSet is the lookup form for write validation.
func (h *GlobalSecretsHandler) sourceSet() map[string]bool {
	srcs := h.sourcesFn()
	set := make(map[string]bool, len(srcs))
	for _, s := range srcs {
		set[s] = true
	}
	return set
}

type secretRefInput struct {
	Path string `json:"path"`
	Key  string `json:"key"`
}

type setGlobalSecretRequest struct {
	Name   string          `json:"name"`
	Value  string          `json:"value"`
	Source string          `json:"source"`
	Ref    *secretRefInput `json:"ref"`
}

type globalSecretsListResponse struct {
	Secrets           []store.Secret `json:"secrets"`
	Total             int64          `json:"total"`
	Limit             int32          `json:"limit"`
	Offset            int32          `json:"offset"`
	ConfiguredSources []string       `json:"configured_sources"`
}

// Set handles POST /api/v1/admin/secrets — create/overwrite a global secret
// (db value or external reference). 201 on insert, 200 on update.
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
	source, refPath, refKey := normalizeSecretWrite(req.Source, req.Ref)
	if err := store.ValidateSecretRef(source, refPath, refKey, h.sourceSet()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if source == store.SecretSourceDB && h.cipher == nil {
		http.Error(w, "secrets disabled: GOCDNEXT_SECRET_KEY must be set for db-stored secrets", http.StatusServiceUnavailable)
		return
	}

	created, err := h.store.SetGlobalSecret(r.Context(), h.cipher, store.SecretSet{
		Name:    req.Name,
		Source:  source,
		Value:   []byte(req.Value),
		RefPath: refPath,
		RefKey:  refKey,
	})
	if err != nil {
		h.log.Error("set global secret", "name", req.Name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.log.Info("global secret set", "name", req.Name, "source", source, "created", created)
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionGlobalSecretSet, "global_secret", req.Name,
		map[string]any{"name": req.Name, "source": source, "created": created})

	w.Header().Set("Content-Type", "application/json")
	if created {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"name": req.Name, "created": created})
}

// List handles GET /api/v1/admin/secrets — paginated, value-free.
func (h *GlobalSecretsHandler) List(w http.ResponseWriter, r *http.Request) {
	if !h.ensureConfigured(w) {
		return
	}
	limit, offset := parseLimitOffset(r)
	page, err := h.store.ListGlobalSecretsPaged(r.Context(), limit, offset)
	if err != nil {
		h.log.Error("list global secrets", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(globalSecretsListResponse{
		Secrets:           page.Secrets,
		Total:             page.Total,
		Limit:             page.Limit,
		Offset:            page.Offset,
		ConfiguredSources: h.writableSources(),
	})
}

// writableSources is the set of sources a write may pick on THIS server: db
// only when a cipher is configured, plus every enabled external backend. The
// UI uses it to gate the source selector so it never offers db on an
// external-only deployment (which the server would 503).
func (h *GlobalSecretsHandler) writableSources() []string {
	srcs := h.configuredSources()
	out := make([]string, 0, len(srcs)+1)
	if h.cipher != nil {
		out = append(out, store.SecretSourceDB)
	}
	return append(out, srcs...)
}

// Delete handles DELETE /api/v1/admin/secrets/{name}. 404 when no match.
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
		audit.Emit(r.Context(), h.log, h.store,
			store.AuditActionGlobalSecretDelete, "global_secret", name,
			map[string]any{"name": name})
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrSecretNotFound):
		http.Error(w, "secret not found", http.StatusNotFound)
	default:
		h.log.Error("delete global secret", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// ensureConfigured allows the endpoint when the server can store a db secret
// (cipher set) OR resolve an external one (a backend configured).
func (h *GlobalSecretsHandler) ensureConfigured(w http.ResponseWriter) bool {
	if h.cipher == nil && len(h.configuredSources()) == 0 {
		http.Error(w,
			"secrets disabled: set GOCDNEXT_SECRET_KEY or enable an external backend",
			http.StatusServiceUnavailable)
		return false
	}
	return true
}

// normalizeSecretWrite trims source/path/key (a stray copy-paste space would
// otherwise become a different reference + cache key that's hard to spot),
// defaults an empty source to db, and flattens the optional ref.
func normalizeSecretWrite(source string, ref *secretRefInput) (string, string, string) {
	source = strings.TrimSpace(source)
	if source == "" {
		source = store.SecretSourceDB
	}
	if ref == nil {
		return source, "", ""
	}
	return source, strings.TrimSpace(ref.Path), strings.TrimSpace(ref.Key)
}

// parseLimitOffset reads ?limit (default 50, 1..200) + ?offset (>=0), the
// audit/webhooks pagination convention.
func parseLimitOffset(r *http.Request) (int32, int32) {
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
