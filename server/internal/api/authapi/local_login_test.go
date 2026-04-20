package authapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/auth"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newLocalHandler(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := authapi.NewHandler(authapi.Config{
		Registry:   auth.NewRegistry(),
		Store:      s,
		Logger:     quiet(),
		PublicBase: "http://localhost:8153",
		DevMode:    true,
	})
	r := chi.NewRouter()
	h.Mount(r)
	return s, r
}

func TestLocalLogin_HappyPath(t *testing.T) {
	s, srv := newLocalHandler(t)
	if _, err := s.CreateOrUpdateLocalUser(context.Background(), "admin@example.com", "Admin", store.RoleAdmin, "hunter2pass"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"email": "admin@example.com", "password": "hunter2pass"})
	req := httptest.NewRequest(http.MethodPost, "/auth/login/local", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		User struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.User.Email != "admin@example.com" || got.User.Role != "admin" {
		t.Fatalf("user = %+v", got.User)
	}

	// Cookie must be set.
	var hasCookie bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == "gocdnext_session" && c.Value != "" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Fatalf("no session cookie minted")
	}
}

func TestLocalLogin_WrongPassword(t *testing.T) {
	s, srv := newLocalHandler(t)
	_, _ = s.CreateOrUpdateLocalUser(context.Background(), "a@example.com", "A", store.RoleAdmin, "rightpass1")
	body, _ := json.Marshal(map[string]string{"email": "a@example.com", "password": "wrongpass"})
	req := httptest.NewRequest(http.MethodPost, "/auth/login/local", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestLocalLogin_RateLimits(t *testing.T) {
	s, srv := newLocalHandler(t)
	_, _ = s.CreateOrUpdateLocalUser(context.Background(), "rl@example.com", "R", store.RoleUser, "correct1pw")
	hit := func() int {
		body, _ := json.Marshal(map[string]string{"email": "rl@example.com", "password": "wrong"})
		req := httptest.NewRequest(http.MethodPost, "/auth/login/local", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr.Code
	}
	// 5 wrong answers trip the limiter (default threshold).
	for i := 0; i < 5; i++ {
		if c := hit(); c != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d", i, c)
		}
	}
	if c := hit(); c != http.StatusTooManyRequests {
		t.Fatalf("6th attempt status = %d, want 429", c)
	}
}

func TestProviders_AdvertisesLocalEnabled(t *testing.T) {
	s, srv := newLocalHandler(t)
	// No local users yet → local_enabled false.
	req := httptest.NewRequest(http.MethodGet, "/auth/providers", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["local_enabled"] != false || got["enabled"] != false {
		t.Fatalf("pre-seed payload = %+v", got)
	}

	_, _ = s.CreateOrUpdateLocalUser(context.Background(), "admin@example.com", "Admin", store.RoleAdmin, "goodpass1")
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["local_enabled"] != true || got["enabled"] != true {
		t.Fatalf("post-seed payload = %+v", got)
	}
}

func TestChangeOwnPassword_HappyPath(t *testing.T) {
	s, srv := newLocalHandler(t)
	u, _ := s.CreateOrUpdateLocalUser(context.Background(), "a@example.com", "A", store.RoleAdmin, "oldpass99")

	body, _ := json.Marshal(map[string]string{
		"current_password": "oldpass99",
		"new_password":     "newpass99",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Inject the user into context (skip the session dance).
	ctx := authapi.WithUser(req.Context(), u)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	// New pw works, old doesn't.
	if _, err := s.AuthenticateLocalUser(context.Background(), "a@example.com", "newpass99"); err != nil {
		t.Fatalf("auth with new pw: %v", err)
	}
}

func TestChangeOwnPassword_Unauthenticated(t *testing.T) {
	_, srv := newLocalHandler(t)
	body, _ := json.Marshal(map[string]string{
		"current_password": "x", "new_password": "yyyyyyyy",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestChangeOwnPassword_OIDCUserRejected(t *testing.T) {
	s, srv := newLocalHandler(t)
	u, _ := s.UpsertUserByProvider(context.Background(), store.UpsertUserInput{
		Email: "gh@example.com", Name: "gh",
		Provider: "github", ExternalID: "999",
	})
	body, _ := json.Marshal(map[string]string{
		"current_password": "x", "new_password": "yyyyyyyy",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := authapi.WithUser(req.Context(), u)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}
