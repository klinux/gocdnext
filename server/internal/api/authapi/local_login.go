package authapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// LocalLogin handles POST /auth/login/local with a JSON
// {email,password} body. Happy path mints a session cookie in the
// same shape as the OIDC callback so the browser + middleware
// code is identical regardless of which provider signed the user
// in. Unhappy path is rate-limited per email.
func (h *Handler) LocalLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32*1024))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" || body.Password == "" {
		http.Error(w, "email and password required", http.StatusBadRequest)
		return
	}

	if !h.localRL.Allow(email) {
		http.Error(w, "too many attempts; try again later", http.StatusTooManyRequests)
		return
	}

	user, err := h.cfg.Store.AuthenticateLocalUser(r.Context(), email, body.Password)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrLocalUserNotFound),
			errors.Is(err, store.ErrLocalPasswordMismatch):
			h.localRL.RecordFailure(email)
			// Identical message on both failures — don't leak
			// which emails exist.
			http.Error(w, "invalid email or password", http.StatusUnauthorized)
			return
		case errors.Is(err, store.ErrUserDisabled):
			http.Error(w, "account disabled", http.StatusForbidden)
			return
		default:
			h.cfg.Logger.Error("local login", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	token, hash, err := store.NewSessionToken()
	if err != nil {
		h.cfg.Logger.Error("local login: session token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ua := r.Header.Get("User-Agent")
	if len(ua) > 500 {
		ua = ua[:500]
	}
	if err := h.cfg.Store.InsertUserSession(r.Context(), hash, user.ID, store.SessionTTL, ua); err != nil {
		h.cfg.Logger.Error("local login: insert session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.localRL.RecordSuccess(email)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  time.Now().Add(store.SessionTTL),
		HttpOnly: true,
		Secure:   !h.cfg.DevMode,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

// ChangeOwnPassword handles POST /api/v1/me/password with a JSON
// {current_password,new_password} body. Only local users can
// change their password here — OIDC users delegate that to the
// IdP. The caller must already have a valid session; the session
// middleware puts the user into context before this runs.
func (h *Handler) ChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	me, ok := UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	if me.Provider != store.ProviderLocal {
		http.Error(w, "password change is only for local accounts", http.StatusForbidden)
		return
	}

	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32*1024))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.CurrentPassword == "" || body.NewPassword == "" {
		http.Error(w, "current_password and new_password required", http.StatusBadRequest)
		return
	}

	// Re-auth with the current password so a stolen session
	// cookie can't rotate the credential silently. Rate-limit
	// applies here too.
	if !h.localRL.Allow(me.Email) {
		http.Error(w, "too many attempts; try again later", http.StatusTooManyRequests)
		return
	}
	if _, err := h.cfg.Store.AuthenticateLocalUser(r.Context(), me.Email, body.CurrentPassword); err != nil {
		if errors.Is(err, store.ErrLocalPasswordMismatch) || errors.Is(err, store.ErrLocalUserNotFound) {
			h.localRL.RecordFailure(me.Email)
			http.Error(w, "current password incorrect", http.StatusUnauthorized)
			return
		}
		h.cfg.Logger.Error("change password: reauth", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.cfg.Store.UpdateLocalUserPassword(r.Context(), me.ID, body.NewPassword); err != nil {
		if errors.Is(err, store.ErrPasswordPolicy) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.cfg.Logger.Error("change password: update", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.localRL.RecordSuccess(me.Email)
	w.WriteHeader(http.StatusNoContent)
}
