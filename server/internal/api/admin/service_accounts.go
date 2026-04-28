package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/auth/apitoken"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// service_accounts.go owns the admin CRUD for service accounts +
// their tokens. Routes:
//
//	GET    /api/v1/admin/service-accounts
//	POST   /api/v1/admin/service-accounts                       { name, description, role }
//	PUT    /api/v1/admin/service-accounts/{id}                  { description, role }
//	DELETE /api/v1/admin/service-accounts/{id}
//	POST   /api/v1/admin/service-accounts/{id}/disable          { disabled: true|false }
//	GET    /api/v1/admin/service-accounts/{id}/tokens
//	POST   /api/v1/admin/service-accounts/{id}/tokens           { name, expires_at? }
//	DELETE /api/v1/admin/service-accounts/{id}/tokens/{tokenID}

type saView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Role        string     `json:"role"`
	CreatedBy   *string    `json:"created_by,omitempty"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func toSAView(sa store.ServiceAccount) saView {
	out := saView{
		ID:          sa.ID.String(),
		Name:        sa.Name,
		Description: sa.Description,
		Role:        sa.Role,
		DisabledAt:  sa.DisabledAt,
		CreatedAt:   sa.CreatedAt,
		UpdatedAt:   sa.UpdatedAt,
	}
	if sa.CreatedBy != nil {
		s := sa.CreatedBy.String()
		out.CreatedBy = &s
	}
	return out
}

// ListServiceAccounts admin endpoint.
func (h *Handler) ListServiceAccounts(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.ListServiceAccounts(r.Context())
	if err != nil {
		h.log.Error("list service accounts", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]saView, 0, len(rows))
	for _, sa := range rows {
		out = append(out, toSAView(sa))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"service_accounts": out})
}

type createSARequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Role        string `json:"role"`
}

// CreateServiceAccount admin endpoint.
func (h *Handler) CreateServiceAccount(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req createSARequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Role = strings.ToLower(strings.TrimSpace(req.Role))
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if !validRole(req.Role) {
		http.Error(w, "role must be one of: admin, maintainer, viewer", http.StatusBadRequest)
		return
	}
	var createdBy *uuid.UUID
	if u, ok := authapi.UserFromContext(r.Context()); ok && u.Provider != "service_account" {
		// SAs creating SAs is legal at the data layer but we want
		// the audit trail to reflect "human authored"; only stamp
		// the creator when the actor is actually a user.
		id := u.ID
		createdBy = &id
	}
	sa, err := h.store.CreateServiceAccount(r.Context(), req.Name, req.Description, req.Role, createdBy)
	if err != nil {
		// Postgres unique-violation = friendly 409.
		if isUniqueViolation(err) {
			http.Error(w, "name already taken", http.StatusConflict)
			return
		}
		h.log.Error("create service account", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionServiceAccountCreate, "service_account", sa.ID.String(),
		map[string]any{"name": req.Name, "role": req.Role})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toSAView(sa))
}

type updateSARequest struct {
	Description string `json:"description"`
	Role        string `json:"role"`
}

// UpdateServiceAccount admin endpoint.
func (h *Handler) UpdateServiceAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req updateSARequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Role = strings.ToLower(strings.TrimSpace(req.Role))
	if !validRole(req.Role) {
		http.Error(w, "role must be one of: admin, maintainer, viewer", http.StatusBadRequest)
		return
	}
	if err := h.store.UpdateServiceAccount(r.Context(), id, req.Description, req.Role); err != nil {
		h.log.Error("update service account", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionServiceAccountUpdate, "service_account", id.String(),
		map[string]any{"role": req.Role})
	w.WriteHeader(http.StatusNoContent)
}

type disableSARequest struct {
	Disabled bool `json:"disabled"`
}

// DisableServiceAccount admin endpoint — body { disabled: true }
// disables, { disabled: false } re-enables.
func (h *Handler) DisableServiceAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, r)
	if !ok {
		return
	}
	var req disableSARequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	var t *time.Time
	if req.Disabled {
		now := time.Now().UTC()
		t = &now
	}
	if err := h.store.SetServiceAccountDisabled(r.Context(), id, t); err != nil {
		h.log.Error("disable service account", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteServiceAccount admin endpoint. Cascades to api_tokens.
func (h *Handler) DeleteServiceAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, r)
	if !ok {
		return
	}
	if err := h.store.DeleteServiceAccount(r.Context(), id); err != nil {
		h.log.Error("delete service account", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionServiceAccountDelete, "service_account", id.String(),
		nil)
	w.WriteHeader(http.StatusNoContent)
}

type saTokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ListSATokens admin endpoint.
func (h *Handler) ListSATokens(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, r)
	if !ok {
		return
	}
	tokens, err := h.store.ListAPITokensForSA(r.Context(), id)
	if err != nil {
		h.log.Error("list sa tokens", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]saTokenView, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, saTokenView{
			ID: t.ID.String(), Name: t.Name, Prefix: t.Prefix,
			ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt,
			RevokedAt: t.RevokedAt, CreatedAt: t.CreatedAt,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tokens": out})
}

type createSATokenRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type createSATokenResponse struct {
	Token     saTokenView `json:"token"`
	Plaintext string      `json:"plaintext"`
}

// CreateSAToken admin endpoint — show-once plaintext.
func (h *Handler) CreateSAToken(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req createSATokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.ExpiresAt != nil && req.ExpiresAt.Before(time.Now()) {
		http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
		return
	}
	gen, err := apitoken.NewSA()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	t, err := h.store.CreateSAAPIToken(r.Context(), id, req.Name, gen.Hash, gen.Prefix, req.ExpiresAt)
	if err != nil {
		h.log.Error("create sa token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionServiceAccountToken, "api_token", t.ID.String(),
		map[string]any{"service_account_id": id.String(), "name": req.Name})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(createSATokenResponse{
		Token: saTokenView{
			ID: t.ID.String(), Name: t.Name, Prefix: t.Prefix,
			ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt,
			RevokedAt: t.RevokedAt, CreatedAt: t.CreatedAt,
		},
		Plaintext: gen.Plaintext,
	})
}

// RevokeSAToken admin endpoint.
func (h *Handler) RevokeSAToken(w http.ResponseWriter, r *http.Request) {
	saID, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	tokenID, ok := parseUUIDParam(w, r, "tokenID")
	if !ok {
		return
	}
	if err := h.store.RevokeSAAPIToken(r.Context(), tokenID, saID); err != nil {
		if errors.Is(err, store.ErrAPITokenNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.log.Error("revoke sa token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionAPITokenRevoke, "api_token", tokenID.String(),
		map[string]any{"service_account_id": saID.String(), "subject": "service_account"})
	w.WriteHeader(http.StatusNoContent)
}

func parseUUID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	return parseUUIDParam(w, r, "id")
}

func parseUUIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}

func validRole(r string) bool {
	switch r {
	case "admin", "maintainer", "viewer":
		return true
	}
	return false
}

// isUniqueViolation matches Postgres SQLSTATE 23505. Used to map
// "name already taken" to a friendly 409. Lifted to its own
// helper so the dependency on pgconn stays here in the handler
// rather than leaking into the store layer.
func isUniqueViolation(err error) bool {
	type sqlState interface{ SQLState() string }
	var s sqlState
	if errors.As(err, &s) {
		return s.SQLState() == "23505"
	}
	return false
}
