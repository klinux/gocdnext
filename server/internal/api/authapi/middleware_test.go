package authapi_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func seedUserSession(t *testing.T, s *store.Store, role string) (user store.User, cookie string) {
	t.Helper()
	u, err := s.UpsertUserByProvider(t.Context(), store.UpsertUserInput{
		Email:       "t@example.com",
		Name:        "T",
		Provider:    "github",
		ExternalID:  "1",
		InitialRole: role,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	token, hash, err := store.NewSessionToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if err := s.InsertUserSession(t.Context(), hash, u.ID, store.SessionTTL, "test"); err != nil {
		t.Fatalf("session: %v", err)
	}
	return u, token
}

func TestMiddleware_Disabled_PassesThrough(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	m := authapi.NewMiddleware(s, quiet(), false)

	h := m.LoadSession(m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := authapi.UserFromContext(r.Context())
		if ok {
			t.Fatalf("user should not be set when auth disabled")
		}
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (auth disabled should pass through)", rr.Code)
	}
}

func TestMiddleware_Enabled_NoCookie_401(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	m := authapi.NewMiddleware(s, quiet(), true)

	h := m.LoadSession(m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("handler should not be reached")
	})))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestMiddleware_ValidSession_SetsUser(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	m := authapi.NewMiddleware(s, quiet(), true)
	u, token := seedUserSession(t, s, store.RoleUser)

	var seenEmail string
	h := m.LoadSession(m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := authapi.UserFromContext(r.Context())
		if !ok {
			t.Fatalf("user not in context")
		}
		seenEmail = got.Email
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: "gocdnext_session", Value: token})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if seenEmail != u.Email {
		t.Fatalf("email = %q, want %q", seenEmail, u.Email)
	}
}

func TestMiddleware_InvalidCookie_ClearsAndPassesAsAnonymous(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	m := authapi.NewMiddleware(s, quiet(), true)

	var anonymous bool
	// LoadSession is the only middleware here — handler checks that
	// no user made it through, then succeeds. This mirrors public
	// routes behind LoadSession (but no RequireAuth).
	h := m.LoadSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := authapi.UserFromContext(r.Context())
		anonymous = !ok
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: "gocdnext_session", Value: "bogus-token"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !anonymous {
		t.Fatalf("bogus cookie leaked a user into context")
	}
	// The middleware should have written a Set-Cookie that clears
	// the bogus one so the browser stops sending it.
	found := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == "gocdnext_session" && c.MaxAge < 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected cookie-clear header")
	}
}

func TestMiddleware_RequireRole_Admin(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	m := authapi.NewMiddleware(s, quiet(), true)

	// user role → 403
	_, userToken := seedUserSession(t, s, store.RoleUser)

	h := m.LoadSession(m.RequireRole("admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: "gocdnext_session", Value: userToken})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin got status %d, want 403", rr.Code)
	}

	// Promote via SQL and retry — the middleware fetches role on
	// every request via GetUserSession, so the very next call sees
	// admin without a re-login.
	if _, err := pool.Exec(t.Context(), `UPDATE users SET role = 'admin' WHERE role = 'user'`); err != nil {
		t.Fatalf("promote: %v", err)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: "gocdnext_session", Value: userToken})
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin got status %d, want 200", rr.Code)
	}
}
