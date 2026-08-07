package authapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/auth/apitoken"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// TestMiddleware_SABearer_Authenticates pins the machine-to-machine
// path documented in docs/install/api-tokens.md: a live service
// account token in `Authorization: Bearer` must satisfy RequireAuth
// with no session cookie present.
func TestMiddleware_SABearer_Authenticates(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	m := authapi.NewMiddleware(s, quiet(), true)
	ctx := t.Context()

	sa, err := s.CreateServiceAccount(ctx, "ci-bot", "automation", store.RoleMaintainer, nil)
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	gen, err := apitoken.NewSA()
	if err != nil {
		t.Fatalf("NewSA: %v", err)
	}
	if _, err := s.CreateSAAPIToken(ctx, sa.ID, "primary", gen.Hash, gen.Prefix, nil); err != nil {
		t.Fatalf("CreateSAAPIToken: %v", err)
	}

	var seen store.User
	h := m.LoadSession(m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = authapi.UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer "+gen.Plaintext)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	if seen.Provider != "service_account" || seen.Role != store.RoleMaintainer {
		t.Fatalf("identity = %+v, want SA maintainer", seen)
	}
}

// TestMiddleware_SABearer_AuthDisabled_StillIdentifies pins the
// case that bites every fresh install: GOCDNEXT_AUTH_ENABLED
// defaults to false, and LoadSession used to skip the Bearer path
// entirely in that mode. Routes stayed open, but any handler that
// needs an identity (/api/v1/me, /api/v1/account/*, rollout gate
// decisions) 401'd with a perfectly valid token — and the CLI's
// `login` reported "token rejected (expired or revoked?)".
//
// Auth disabled must keep meaning "don't REQUIRE a session", not
// "ignore credentials that were presented".
func TestMiddleware_SABearer_AuthDisabled_StillIdentifies(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	m := authapi.NewMiddleware(s, quiet(), false)
	ctx := t.Context()

	sa, err := s.CreateServiceAccount(ctx, "ci-bot", "automation", store.RoleMaintainer, nil)
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	gen, err := apitoken.NewSA()
	if err != nil {
		t.Fatalf("NewSA: %v", err)
	}
	if _, err := s.CreateSAAPIToken(ctx, sa.ID, "primary", gen.Hash, gen.Prefix, nil); err != nil {
		t.Fatalf("CreateSAAPIToken: %v", err)
	}

	// Stand-in for authapi.Handler.Me: 401s when no identity is in
	// context, regardless of whether auth enforcement is on.
	h := m.LoadSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authapi.UserFromContext(r.Context()); !ok {
			http.Error(w, "not authenticated", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+gen.Plaintext)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — valid SA token ignored with auth disabled", rr.Code)
	}
}

// TestMiddleware_AuthDisabled_NoToken_StaysAnonymous guards the
// other half: with enforcement off and no credentials presented,
// requests must still sail through anonymously.
func TestMiddleware_AuthDisabled_NoToken_StaysAnonymous(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	m := authapi.NewMiddleware(s, quiet(), false)

	h := m.LoadSession(m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authapi.UserFromContext(r.Context()); ok {
			t.Errorf("anonymous request got an identity")
		}
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

// TestMiddleware_AuthDisabled_BogusToken_StaysAnonymous: a garbage
// bearer must not turn an open deployment into a closed one.
func TestMiddleware_AuthDisabled_BogusToken_StaysAnonymous(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	m := authapi.NewMiddleware(s, quiet(), false)

	h := m.LoadSession(m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer gnk_sa_NOPENOPENOPE")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (auth disabled must stay open)", rr.Code)
	}
}

// TestMiddleware_UserBearer_Authenticates is the per-user twin.
func TestMiddleware_UserBearer_Authenticates(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	m := authapi.NewMiddleware(s, quiet(), true)
	ctx := t.Context()

	u, err := s.UpsertUserByProvider(ctx, store.UpsertUserInput{
		Email: "bearer@example.com", Name: "B",
		Provider: "github", ExternalID: "77",
		InitialRole: store.RoleMaintainer,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	gen, err := apitoken.NewUser()
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	if _, err := s.CreateUserAPIToken(ctx, u.ID, "laptop", gen.Hash, gen.Prefix, nil); err != nil {
		t.Fatalf("CreateUserAPIToken: %v", err)
	}

	h := m.LoadSession(m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer "+gen.Plaintext)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
}
