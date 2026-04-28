package authapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/auth/apitoken"
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
//
// Two paths in priority order:
//
//  1. `Authorization: Bearer gnk_...` — API token (user or
//     service account). The token's hash hits api_tokens; the
//     subject becomes the request's identity. Service accounts
//     surface as a synthetic User with Provider="service_account"
//     so downstream RBAC works without code changes — audit
//     lines read "<name>@service-account" cleanly.
//  2. Session cookie — the browser path. Same as before.
//
// Bearer takes precedence: when a token is present + valid we
// don't read the cookie. Bearer present but invalid (revoked,
// expired, malformed) falls through to cookie / anonymous;
// RequireAuth on protected routes 401s as usual.
func (m *Middleware) LoadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.enabled {
			next.ServeHTTP(w, r)
			return
		}

		if u, ok := m.authenticateBearer(r); ok {
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
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

// authenticateBearer parses an Authorization: Bearer header and,
// if it points at a live api_tokens row, returns the subject as
// a store.User. Misses (no header, malformed, unknown hash,
// revoked, expired, disabled SA) all return ok=false silently —
// the caller falls through to cookie / anonymous. DB hiccups
// log a warn and treat as miss; we don't want a transient flap
// to lock out every machine.
func (m *Middleware) authenticateBearer(r *http.Request) (store.User, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return store.User{}, false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return store.User{}, false
	}
	bearer := strings.TrimSpace(header[len(prefix):])
	kind, body, err := apitoken.Parse(bearer)
	if err != nil {
		// Not our token shape; might be an OAuth bearer bound for
		// a different middleware. Silent miss.
		return store.User{}, false
	}
	hash := apitoken.Hash(body)
	row, err := m.store.LookupAPITokenByHash(r.Context(), hash)
	if errors.Is(err, store.ErrAPITokenNotFound) {
		return store.User{}, false
	}
	if err != nil {
		m.log.Warn("auth middleware: api token lookup", "err", err)
		return store.User{}, false
	}

	switch row.Subject {
	case store.TokenSubjectUser:
		if kind != apitoken.KindUser {
			// Token prefix says user but the row points at a SA,
			// or vice versa. Treat as miss.
			return store.User{}, false
		}
		u, err := m.store.GetUser(r.Context(), row.SubjectID)
		if err != nil {
			m.log.Warn("auth middleware: user lookup for token", "err", err)
			return store.User{}, false
		}
		if u.DisabledAt != nil {
			return store.User{}, false
		}
		_ = m.store.TouchAPITokenLastUsed(r.Context(), row.ID)
		return u, true

	case store.TokenSubjectServiceAccount:
		if kind != apitoken.KindSA {
			return store.User{}, false
		}
		sa, err := m.store.GetServiceAccount(r.Context(), row.SubjectID)
		if err != nil {
			m.log.Warn("auth middleware: sa lookup for token", "err", err)
			return store.User{}, false
		}
		if sa.DisabledAt != nil {
			return store.User{}, false
		}
		_ = m.store.TouchAPITokenLastUsed(r.Context(), row.ID)
		// Synthesize a User from the SA. Downstream RBAC reads
		// Role; audit reads Email/Provider so SAs are
		// distinguishable from real users in the audit log.
		return store.User{
			ID:       sa.ID,
			Email:    sa.Name + "@service-account",
			Name:     sa.Name,
			Provider: "service_account",
			Role:     sa.Role,
		}, true
	}
	return store.User{}, false
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

// RequireRole chains RequireAuth + an EXACT role allow-list.
// Use this when "only admin can rotate encryption keys" — literally
// that role, nothing else. For the common "at least maintainer"
// shape use RequireMinRole which respects the hierarchy.
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

// RequireMinRole chains RequireAuth + a hierarchical role check
// using store.RoleSatisfies. admin ≥ maintainer ≥ viewer, so
// `RequireMinRole(RoleMaintainer)` lets both admin and maintainer
// through. Call sites gain clarity over `RequireRole(admin,
// maintainer)` and don't risk drifting when a new tier lands.
func (m *Middleware) RequireMinRole(min string) func(http.Handler) http.Handler {
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
			if !store.RoleSatisfies(u.Role, min) {
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
