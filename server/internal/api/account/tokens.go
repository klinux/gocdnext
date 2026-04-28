package account

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

// TokenStore is the slice of *store.Store the per-user token
// handler needs. Lifted to an interface so tests can drive the
// handler without a real DB.
type TokenStore interface {
	CreateUserAPIToken(ctx context.Context, userID uuid.UUID, name, hash, prefix string, expiresAt *time.Time) (store.APIToken, error)
	ListAPITokensForUser(ctx context.Context, userID uuid.UUID) ([]store.APIToken, error)
	RevokeUserAPIToken(ctx context.Context, tokenID, userID uuid.UUID) error
}

// TokensHandler hangs off the same /api/v1/account/* mount so a
// signed-in user manages "their stuff" through one tree. Routes:
//
//	GET    /api/v1/account/api-tokens
//	POST   /api/v1/account/api-tokens          { name, expires_at? }
//	DELETE /api/v1/account/api-tokens/{id}
type TokensHandler struct {
	store     TokenStore
	auditSink *store.Store // for audit.Emit; may be nil in tests
	log       *slog.Logger
}

// NewTokensHandler wires the per-user token handler. `s` is the
// narrow interface the handler actually uses; `auditSink` is the
// full *store.Store needed by audit.Emit (nil = audit disabled,
// useful in tests).
func NewTokensHandler(s TokenStore, auditSink *store.Store, log *slog.Logger) *TokensHandler {
	if log == nil {
		log = slog.Default()
	}
	return &TokensHandler{store: s, auditSink: auditSink, log: log}
}

type tokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func toTokenView(t store.APIToken) tokenView {
	return tokenView{
		ID:         t.ID.String(),
		Name:       t.Name,
		Prefix:     t.Prefix,
		ExpiresAt:  t.ExpiresAt,
		LastUsedAt: t.LastUsedAt,
		RevokedAt:  t.RevokedAt,
		CreatedAt:  t.CreatedAt,
	}
}

// ListTokens returns the caller's tokens. Service-account
// "users" (Provider="service_account") get a 403 — SAs don't
// own tokens through the per-user API; admins manage SA tokens
// via the admin endpoints.
func (h *TokensHandler) ListTokens(w http.ResponseWriter, r *http.Request) {
	u, ok := authapi.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	if u.Provider == "service_account" {
		http.Error(w, "service accounts manage tokens via /admin/service-accounts", http.StatusForbidden)
		return
	}
	tokens, err := h.store.ListAPITokensForUser(r.Context(), u.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]tokenView, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, toTokenView(t))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tokens": out})
}

type createTokenRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type createTokenResponse struct {
	Token     tokenView `json:"token"`
	// Plaintext is shown to the user EXACTLY ONCE — they copy it
	// to a password manager + we never see it again. Subsequent
	// reads through ListTokens omit this field by design.
	Plaintext string `json:"plaintext"`
}

// CreateToken mints a fresh user-owned token. The plaintext is
// returned in the response body and never persisted — the API
// is "show-once".
func (h *TokensHandler) CreateToken(w http.ResponseWriter, r *http.Request) {
	u, ok := authapi.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	if u.Provider == "service_account" {
		http.Error(w, "service accounts manage tokens via /admin/service-accounts", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req createTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Name = trimNonEmpty(req.Name)
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.ExpiresAt != nil && req.ExpiresAt.Before(time.Now()) {
		http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
		return
	}

	gen, err := apitoken.NewUser()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	t, err := h.store.CreateUserAPIToken(r.Context(), u.ID, req.Name, gen.Hash, gen.Prefix, req.ExpiresAt)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if h.auditSink != nil {
		audit.Emit(r.Context(), h.log, h.auditSink,
			store.AuditActionAPITokenCreate, "api_token", t.ID.String(),
			map[string]any{"name": req.Name, "subject": "user"})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(createTokenResponse{
		Token:     toTokenView(t),
		Plaintext: gen.Plaintext,
	})
}

// RevokeToken flips revoked_at on a token the caller owns.
// Idempotent — already-revoked tokens are a 204 anyway.
func (h *TokensHandler) RevokeToken(w http.ResponseWriter, r *http.Request) {
	u, ok := authapi.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	if u.Provider == "service_account" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	idStr := chi.URLParam(r, "id")
	tokenID, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid token id", http.StatusBadRequest)
		return
	}
	if err := h.store.RevokeUserAPIToken(r.Context(), tokenID, u.ID); err != nil {
		if errors.Is(err, store.ErrAPITokenNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if h.auditSink != nil {
		audit.Emit(r.Context(), h.log, h.auditSink,
			store.AuditActionAPITokenRevoke, "api_token", tokenID.String(),
			map[string]any{"subject": "user"})
	}
	w.WriteHeader(http.StatusNoContent)
}

func trimNonEmpty(s string) string {
	return strings.TrimSpace(s)
}
