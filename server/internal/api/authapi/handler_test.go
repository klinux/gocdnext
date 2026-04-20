package authapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/auth"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// fakeProvider is a minimal Provider that lets us drive the full
// login+callback flow without touching an external IdP. Exchange
// returns whatever claims were stashed at construction time.
type fakeProvider struct {
	name    auth.ProviderName
	display string
	claims  auth.Claims
	err     error
}

func (f *fakeProvider) Name() auth.ProviderName { return f.name }
func (f *fakeProvider) DisplayName() string     { return f.display }
func (f *fakeProvider) AuthorizeURL(state, _ string) string {
	return "https://idp.example.com/authorize?state=" + state
}
func (f *fakeProvider) Exchange(context.Context, string, string, string) (auth.Claims, error) {
	return f.claims, f.err
}

func newTestHandler(t *testing.T, providers ...auth.Provider) (*authapi.Handler, *store.Store, http.Handler) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	reg := auth.NewRegistry(providers...)
	h := authapi.NewHandler(authapi.Config{
		Registry:    reg,
		Store:       s,
		Logger:      quiet(),
		PublicBase:  "http://localhost:8153",
		AdminEmails: []string{"admin@example.com"},
		DevMode:     true,
	})
	r := chi.NewRouter()
	h.Mount(r)
	return h, s, r
}

func TestProviders_ListsEnabled(t *testing.T) {
	_, _, srv := newTestHandler(t,
		&fakeProvider{name: auth.ProviderGitHub, display: "GitHub"},
		&fakeProvider{name: auth.ProviderGoogle, display: "Google"},
	)
	req := httptest.NewRequest(http.MethodGet, "/auth/providers", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got struct {
		Enabled   bool              `json:"enabled"`
		Providers []json.RawMessage `json:"providers"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if !got.Enabled || len(got.Providers) != 2 {
		t.Fatalf("payload = %+v", got)
	}
}

func TestLogin_RedirectsToProvider(t *testing.T) {
	_, _, srv := newTestHandler(t,
		&fakeProvider{name: auth.ProviderGitHub, display: "GitHub"},
	)
	req := httptest.NewRequest(http.MethodGet, "/auth/login/github?next=/runs/123", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://idp.example.com/authorize?state=") {
		t.Fatalf("location = %q", loc)
	}
}

func TestLogin_UnknownProvider_404(t *testing.T) {
	_, _, srv := newTestHandler(t,
		&fakeProvider{name: auth.ProviderGitHub, display: "GitHub"},
	)
	req := httptest.NewRequest(http.MethodGet, "/auth/login/gitlab", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestCallback_HappyPath(t *testing.T) {
	claims := auth.Claims{
		Subject:   "42",
		Email:     "alice@example.com",
		Name:      "Alice",
		AvatarURL: "https://cdn/alice.png",
	}
	h, s, srv := newTestHandler(t,
		&fakeProvider{name: auth.ProviderGitHub, display: "GitHub", claims: claims},
	)
	_ = h

	// First: hit /auth/login to mint a state token.
	req := httptest.NewRequest(http.MethodGet, "/auth/login/github?next=/runs/123", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	loc := rr.Header().Get("Location")
	state := extractQueryParam(t, loc, "state")

	// Now hit /auth/callback with that state.
	req = httptest.NewRequest(http.MethodGet, "/auth/callback/github?code=abc&state="+state, nil)
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/runs/123" {
		t.Fatalf("redirect = %q, want /runs/123", got)
	}

	// Cookie should be set.
	var sessionCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "gocdnext_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatalf("no session cookie minted")
	}

	// And the session row exists.
	view, err := s.GetUserSession(context.Background(), store.HashSessionToken(sessionCookie.Value))
	if err != nil {
		t.Fatalf("session lookup: %v", err)
	}
	if view.User.Email != claims.Email {
		t.Fatalf("session user = %+v", view.User)
	}
}

func TestCallback_AdminEmailGetsAdminRole(t *testing.T) {
	claims := auth.Claims{
		Subject: "99",
		Email:   "admin@example.com",
		Name:    "Admin",
	}
	_, s, srv := newTestHandler(t,
		&fakeProvider{name: auth.ProviderGitHub, display: "GitHub", claims: claims},
	)

	state := runLoginFlow(t, srv)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback/github?code=abc&state="+state, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d", rr.Code)
	}

	sessionCookie := findCookie(rr, "gocdnext_session")
	view, err := s.GetUserSession(context.Background(), store.HashSessionToken(sessionCookie.Value))
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if view.User.Role != store.RoleAdmin {
		t.Fatalf("role = %q, want admin", view.User.Role)
	}
}

func TestCallback_BadState_401(t *testing.T) {
	_, _, srv := newTestHandler(t,
		&fakeProvider{name: auth.ProviderGitHub, display: "GitHub"},
	)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback/github?code=abc&state=nonsense", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestCallback_DomainAllowlist_Rejects(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	reg := auth.NewRegistry(&fakeProvider{
		name: auth.ProviderGitHub, display: "GitHub",
		claims: auth.Claims{Subject: "1", Email: "hacker@evil.com", Name: "x"},
	})
	h := authapi.NewHandler(authapi.Config{
		Registry:       reg,
		Store:          s,
		Logger:         quiet(),
		PublicBase:     "http://localhost:8153",
		AllowedDomains: []string{"example.com"},
		DevMode:        true,
	})
	srv := chi.NewRouter()
	h.Mount(srv)

	state := runLoginFlow(t, srv)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback/github?code=abc&state="+state, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestLogout_ClearsCookieAndSession(t *testing.T) {
	_, s, srv := newTestHandler(t,
		&fakeProvider{name: auth.ProviderGitHub, display: "GitHub",
			claims: auth.Claims{Subject: "1", Email: "a@example.com", Name: "A"}},
	)

	state := runLoginFlow(t, srv)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback/github?code=abc&state="+state, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	cookie := findCookie(rr, "gocdnext_session")

	req = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rr.Code)
	}

	// Session should be gone.
	if _, err := s.GetUserSession(context.Background(), store.HashSessionToken(cookie.Value)); err == nil {
		t.Fatalf("session still valid after logout")
	}
	// A cookie-clear header should be present.
	cleared := findCookie(rr, "gocdnext_session")
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("expected cookie-clear, got %+v", cleared)
	}
}

func TestMe_WithoutSession_401(t *testing.T) {
	_, _, srv := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rr.Code)
	}
}

// --- small helpers ---

func runLoginFlow(t *testing.T, srv http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/login/github?next=/", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return extractQueryParam(t, rr.Header().Get("Location"), "state")
}

func extractQueryParam(t *testing.T, rawURL, name string) string {
	t.Helper()
	i := strings.Index(rawURL, "?")
	if i < 0 {
		t.Fatalf("no query on %q", rawURL)
	}
	for _, p := range strings.Split(rawURL[i+1:], "&") {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) == 2 && kv[0] == name {
			return kv[1]
		}
	}
	t.Fatalf("param %q absent in %q", name, rawURL)
	return ""
}

func findCookie(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}
