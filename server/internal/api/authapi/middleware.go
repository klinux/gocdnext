package authapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

type ctxKey int

const (
	userCtxKey ctxKey = iota
)

// Middleware validates the session cookie on every incoming request.
// When a valid session is found, the user is stuffed into the request
// context for handlers to pull via UserFromContext. Requests without
// a valid session pass through with no user — RequireAuth is the
// separate middleware that 401s on anonymous hits.
//
// Kept distinct from RequireAuth so routes like /auth/login and /
// healthz stay public.
type Middleware struct {
	store  *store.Store
	log    *slog.Logger
	// enabled guards whether session validation actually happens.
	// When false (auth disabled), every request passes anonymously
	// and RequireAuth + RequireRole short-circuit to allow. Lets
	// existing dev deployments keep running with no auth.
	enabled bool
}

// NewMiddleware builds the session-check middleware. enabled=false
// short-circuits every check: the API stays open and /api/v1/me
// returns 401 (callers can poll to detect the mode).
func NewMiddleware(s *store.Store, log *slog.Logger, enabled bool) *Middleware {
	if log == nil {
		log = slog.Default()
	}
	return &Middleware{store: s, log: log, enabled: enabled}
}

// Enabled reports whether the middleware is running in enforcement
// mode. Useful for branching in tests + /auth/providers payloads.
func (m *Middleware) Enabled() bool { return m.enabled }

// LoadSession is a chi-compatible middleware. Call it once on the
// top-level router; it stuffs the user into context so downstream
// handlers + the RequireAuth middleware can read it.
func (m *Middleware) LoadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.enabled {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		hash := store.HashSessionToken(cookie.Value)
		view, err := m.store.GetUserSession(r.Context(), hash)
		if errors.Is(err, store.ErrUserSessionNotFound) || errors.Is(err, store.ErrUserDisabled) {
			// Expired or disabled — clear the cookie so the browser
			// stops sending it. Don't 401 here; public routes still
			// need to work. RequireAuth on protected ones will 401.
			clearCookie(w)
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			m.log.Warn("auth middleware: session lookup", "err", err)
			next.ServeHTTP(w, r)
			return
		}
		// Best-effort bump of last_seen_at. Errors here are
		// non-fatal — the session still works.
		if err := m.store.TouchUserSession(r.Context(), hash); err != nil {
			m.log.Warn("auth middleware: touch session", "err", err)
		}
		ctx := context.WithValue(r.Context(), userCtxKey, view.User)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAuth is applied only on routes that must see a signed-in
// user. Pass-through when auth is globally disabled.
func (m *Middleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.enabled {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := UserFromContext(r.Context()); !ok {
			http.Error(w, "not authenticated", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireRole chains RequireAuth + a role check. admin is strictly
// greater than user > viewer; we compare against an explicit set
// instead of a numeric rank so typos at call sites get caught.
func (m *Middleware) RequireRole(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		allowed[strings.ToLower(r)] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !m.enabled {
				next.ServeHTTP(w, r)
				return
			}
			u, ok := UserFromContext(r.Context())
			if !ok {
				http.Error(w, "not authenticated", http.StatusUnauthorized)
				return
			}
			if _, ok := allowed[strings.ToLower(u.Role)]; !ok {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// UserFromContext pulls the authenticated user from a request
// context. Returns (_, false) on anonymous requests.
func UserFromContext(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userCtxKey).(store.User)
	return u, ok
}

// WithUser injects a user into the context. Exported so tests can
// drive handlers that depend on the authenticated user without
// standing up the session flow.
func WithUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
