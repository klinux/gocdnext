// Package authapi wires HTTP routes for login/logout/callback and a
// me-endpoint, plus the cookie+session middleware the rest of the
// API layer plugs in. The provider logic and session persistence
// live elsewhere — this package is the thin glue.
package authapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/auth"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// sessionCookieName is the single cookie the browser carries. We
// don't expose multiple tokens (e.g. refresh) to the client because
// a refresh flow isn't in this slice — sessions live for SessionTTL
// and re-login is how you refresh.
const sessionCookieName = "gocdnext_session"

// safeRedirectScheme strips redirect_to inputs we don't want: no
// absolute URLs (phishing risk), no scheme, no query params.
var allowedRedirectPrefix = "/"

// Config captures everything the handler needs at boot. The caller
// (main.go) constructs it once; this package stays pure.
type Config struct {
	Registry *auth.Registry
	Store    *store.Store
	Logger   *slog.Logger
	// PublicBase is the externally-reachable https://host of the
	// control plane. Used to build the absolute callback URL the
	// IdPs redirect back to, and to match the cookie's Secure flag
	// to the scheme.
	PublicBase string
	// AllowedDomains gates new users at first login — empty list
	// means "any domain passes". Matched case-insensitively on the
	// email suffix (not the whole address).
	AllowedDomains []string
	// AdminEmails assign role=admin at first login. Matched case-
	// insensitively on the full address.
	AdminEmails []string
	// DevMode drops the Secure flag on cookies so localhost http
	// logins work. NEVER set true in production.
	DevMode bool
}

// Handler owns the auth routes.
type Handler struct {
	cfg            Config
	allowedDomains []string
	adminEmails    []string
	// localRL throttles /auth/login/local so a scanner can't
	// brute-force a weak break-glass password. In-memory, per-
	// process — good enough for a single-binary CI server.
	localRL *loginRateLimiter
}

// NewHandler bakes the config into an idempotent handler.
func NewHandler(cfg Config) *Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	h := &Handler{cfg: cfg, localRL: newLoginRateLimiter()}
	for _, d := range cfg.AllowedDomains {
		h.allowedDomains = append(h.allowedDomains, strings.ToLower(strings.TrimPrefix(d, "@")))
	}
	for _, e := range cfg.AdminEmails {
		h.adminEmails = append(h.adminEmails, strings.ToLower(e))
	}
	return h
}

// Mount registers the auth routes under /auth/* on the given router.
// Path shape: /auth/providers, /auth/login/{provider}, /auth/callback/{provider},
// /auth/logout, /auth/login/local, /api/v1/me. The me-endpoint and
// local-password-change go under /api/v1 so the frontend's single
// fetch layer handles them.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/auth/providers", h.Providers)
	r.Get("/auth/login/{provider}", h.Login)
	r.Get("/auth/callback/{provider}", h.Callback)
	r.Post("/auth/login/local", h.LocalLogin)
	r.Post("/auth/logout", h.Logout)
	r.Get("/api/v1/me", h.Me)
	r.Post("/api/v1/me/password", h.ChangeOwnPassword)
}

// Providers handles GET /auth/providers. Returns the enabled providers
// so the frontend can render buttons. No session required.
func (h *Handler) Providers(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name    string `json:"name"`
		Display string `json:"display"`
	}
	list := h.cfg.Registry.List()
	out := make([]item, 0, len(list))
	for _, p := range list {
		out = append(out, item{Name: string(p.Name()), Display: p.DisplayName()})
	}
	// Check for local users so the UI knows whether to render the
	// password form. A DB failure here is swallowed: the login
	// page still shows the OIDC buttons.
	localEnabled := false
	if has, err := h.cfg.Store.HasLocalUsers(r.Context()); err == nil {
		localEnabled = has
	} else {
		h.cfg.Logger.Warn("auth providers: has local users", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       h.cfg.Registry.Len() > 0 || localEnabled,
		"providers":     out,
		"local_enabled": localEnabled,
	})
}

// Login handles GET /auth/login/{provider}?next=<url>. Mints an
// auth_state, redirects to the IdP. `next` is preserved through the
// flow so the callback can bounce the user back to where they were.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	name := auth.ProviderName(chi.URLParam(r, "provider"))
	prov := h.cfg.Registry.Get(name)
	if prov == nil {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}
	next := sanitizeRedirect(r.URL.Query().Get("next"))
	nonce, err := randomNonce()
	if err != nil {
		h.cfg.Logger.Error("auth nonce", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	state, err := h.cfg.Store.NewAuthState(r.Context(), string(name), next, nonce)
	if err != nil {
		h.cfg.Logger.Error("auth state issue", "provider", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, prov.AuthorizeURL(state, nonce), http.StatusFound)
}

// Callback handles GET /auth/callback/{provider}. Validates state,
// exchanges the code for claims, upserts the user, mints a session
// cookie, and bounces the browser back to `redirect_to` from the
// state row (or "/" if empty).
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	name := auth.ProviderName(chi.URLParam(r, "provider"))
	prov := h.cfg.Registry.Get(name)
	if prov == nil {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	stateData, err := h.cfg.Store.ConsumeAuthState(r.Context(), state)
	if errors.Is(err, store.ErrAuthStateNotFound) {
		http.Error(w, "state expired or already used; retry login", http.StatusUnauthorized)
		return
	}
	if err != nil {
		h.cfg.Logger.Error("auth state consume", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if stateData.Provider != string(name) {
		// Cross-provider state reuse is a sign someone's probing;
		// 401 rather than 400 because it's security-adjacent.
		http.Error(w, "state provider mismatch", http.StatusUnauthorized)
		return
	}

	claims, err := prov.Exchange(r.Context(), code, state, stateData.Nonce)
	if err != nil {
		h.cfg.Logger.Warn("auth exchange", "provider", name, "err", err)
		if errors.Is(err, auth.ErrClaimsMissing) {
			http.Error(w, "provider did not return full profile (email/subject missing)", http.StatusBadGateway)
			return
		}
		http.Error(w, "provider rejected the sign-in", http.StatusBadGateway)
		return
	}

	if !h.emailAllowed(claims.Email) {
		h.cfg.Logger.Warn("auth rejected by domain allowlist",
			"provider", name, "email", claims.Email)
		http.Error(w, "email domain not allowed", http.StatusForbidden)
		return
	}

	role := store.RoleUser
	if h.isAdmin(claims.Email) {
		role = store.RoleAdmin
	}
	user, err := h.cfg.Store.UpsertUserByProvider(r.Context(), store.UpsertUserInput{
		Email:       claims.Email,
		Name:        claims.Name,
		AvatarURL:   claims.AvatarURL,
		Provider:    string(name),
		ExternalID:  claims.Subject,
		InitialRole: role,
	})
	if errors.Is(err, store.ErrUserDisabled) {
		http.Error(w, "this account is disabled", http.StatusForbidden)
		return
	}
	if err != nil {
		h.cfg.Logger.Error("auth upsert user", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	token, hash, err := store.NewSessionToken()
	if err != nil {
		h.cfg.Logger.Error("auth session token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ua := r.Header.Get("User-Agent")
	if len(ua) > 500 {
		ua = ua[:500]
	}
	if err := h.cfg.Store.InsertUserSession(r.Context(), hash, user.ID, store.SessionTTL, ua); err != nil {
		h.cfg.Logger.Error("auth insert session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  time.Now().Add(store.SessionTTL),
		HttpOnly: true,
		Secure:   !h.cfg.DevMode,
		SameSite: http.SameSiteLaxMode,
	})

	dest := sanitizeRedirect(stateData.RedirectTo)
	if dest == "/" {
		dest = "/"
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// Logout handles POST /auth/logout. Clears the cookie and deletes
// the corresponding session row. Idempotent: no cookie = 204.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil && cookie.Value != "" {
		_ = h.cfg.Store.DeleteUserSession(r.Context(), store.HashSessionToken(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   !h.cfg.DevMode,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// Me handles GET /api/v1/me. Returns the authenticated user or 401.
// Wired under the same middleware as every other /api/v1 route, but
// the route itself is cheap — it just hands back the user already
// loaded into request context.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	u, ok := UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// --- helpers ---

func (h *Handler) emailAllowed(email string) bool {
	if len(h.allowedDomains) == 0 {
		return true
	}
	idx := strings.LastIndex(email, "@")
	if idx < 0 {
		return false
	}
	domain := strings.ToLower(email[idx+1:])
	for _, d := range h.allowedDomains {
		if d == domain {
			return true
		}
	}
	return false
}

func (h *Handler) isAdmin(email string) bool {
	e := strings.ToLower(email)
	for _, a := range h.adminEmails {
		if a == e {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// sanitizeRedirect only allows relative paths on the same origin.
// Anything else collapses to "/".
func sanitizeRedirect(raw string) string {
	if raw == "" {
		return "/"
	}
	if !strings.HasPrefix(raw, allowedRedirectPrefix) {
		return "/"
	}
	// Reject protocol-relative URLs: //evil.com
	if strings.HasPrefix(raw, "//") {
		return "/"
	}
	// url.Parse to reject any ?query or embedded auth — we only want
	// paths + optional trailing query, no scheme/host/user.
	u, err := url.Parse(raw)
	if err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	return raw
}
