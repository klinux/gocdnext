// Package account surfaces per-user settings as a small JSON API:
// GET/PUT of the preferences document the projects page and future
// dashboards read to decide what to show. Kept separate from the
// session/authn surface (authapi) so that adding preferences keys
// doesn't risk touching the login flow.
package account

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Store is the subset of *store.Store this handler needs. Keeping
// it narrow lets the test wire a fake without pulling in the full
// read surface.
type Store interface {
	GetUserPreferences(ctx context.Context, userID uuid.UUID) (store.UserPreferences, time.Time, error)
	SetUserPreferences(ctx context.Context, userID uuid.UUID, prefs store.UserPreferences) (store.UserPreferences, time.Time, error)
}

type Handler struct {
	store Store
	log   *slog.Logger
}

func New(s Store, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: s, log: log}
}

type preferencesResponse struct {
	Preferences store.UserPreferences `json:"preferences"`
	UpdatedAt   *time.Time            `json:"updated_at,omitempty"`
}

// GetPreferences returns the current user's preferences document.
// A user that has never saved a preference still gets a successful
// 200 with an empty document — the UI always has something to
// render.
func (h *Handler) GetPreferences(w http.ResponseWriter, r *http.Request) {
	user, ok := authapi.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	prefs, updatedAt, err := h.store.GetUserPreferences(r.Context(), user.ID)
	if err != nil {
		h.log.Error("get user preferences", "user_id", user.ID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	resp := preferencesResponse{Preferences: prefs}
	if !updatedAt.IsZero() {
		resp.UpdatedAt = &updatedAt
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// PutPreferences replaces the current user's preferences document
// with the body. Full-document PUT — the client loads, edits, and
// saves the whole thing. No merge logic on the server.
func (h *Handler) PutPreferences(w http.ResponseWriter, r *http.Request) {
	user, ok := authapi.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	var body struct {
		Preferences store.UserPreferences `json:"preferences"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	saved, updatedAt, err := h.store.SetUserPreferences(r.Context(), user.ID, body.Preferences)
	if err != nil {
		h.log.Error("set user preferences", "user_id", user.ID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	resp := preferencesResponse{Preferences: saved, UpdatedAt: &updatedAt}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
