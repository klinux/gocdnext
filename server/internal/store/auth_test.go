package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestUpsertUserByProvider_InsertThenUpdate(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	first, err := s.UpsertUserByProvider(ctx, store.UpsertUserInput{
		Email:      "alice@example.com",
		Name:       "Alice",
		AvatarURL:  "https://cdn/alice.png",
		Provider:   "github",
		ExternalID: "42",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if first.Role != store.RoleUser {
		t.Fatalf("default role = %q, want %q", first.Role, store.RoleUser)
	}
	if first.LastLoginAt == nil {
		t.Fatalf("last_login_at not set on insert")
	}

	// Promote to admin via direct SQL; second upsert must NOT reset.
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}

	second, err := s.UpsertUserByProvider(ctx, store.UpsertUserInput{
		Email:      "alice-new@example.com", // email change from the IdP
		Name:       "Alice R",
		AvatarURL:  "https://cdn/alice-new.png",
		Provider:   "github",
		ExternalID: "42",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("id changed on upsert")
	}
	if second.Role != store.RoleAdmin {
		t.Fatalf("role reverted to %q; must preserve admin across upserts", second.Role)
	}
	if second.Email != "alice-new@example.com" {
		t.Fatalf("email not refreshed from idp")
	}
}

func TestUpsertUserByProvider_DisabledUser(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	u, err := s.UpsertUserByProvider(ctx, store.UpsertUserInput{
		Email: "bob@example.com", Name: "Bob",
		Provider: "google", ExternalID: "bob-ext",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled_at = NOW() WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}

	_, err = s.UpsertUserByProvider(ctx, store.UpsertUserInput{
		Email: "bob@example.com", Name: "Bob",
		Provider: "google", ExternalID: "bob-ext",
	})
	if !errors.Is(err, store.ErrUserDisabled) {
		t.Fatalf("err = %v, want ErrUserDisabled", err)
	}
}

func TestAuthState_IssueAndConsumeOnce(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	state, err := s.NewAuthState(ctx, "github", "/settings/health", "nonce-123")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	if state == "" {
		t.Fatalf("state token empty")
	}

	out, err := s.ConsumeAuthState(ctx, state)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if out.Provider != "github" {
		t.Fatalf("provider = %q", out.Provider)
	}
	if out.RedirectTo != "/settings/health" {
		t.Fatalf("redirect_to = %q", out.RedirectTo)
	}
	if out.Nonce != "nonce-123" {
		t.Fatalf("nonce = %q", out.Nonce)
	}

	// Single-use: second consume fails.
	if _, err := s.ConsumeAuthState(ctx, state); !errors.Is(err, store.ErrAuthStateNotFound) {
		t.Fatalf("second consume err = %v, want ErrAuthStateNotFound", err)
	}
}

func TestAuthState_ConsumeExpired(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	state, err := s.NewAuthState(ctx, "google", "", "")
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	// Age it past TTL by direct update.
	hash := store.HashSessionToken(state) // same sha256 truncation pattern
	if _, err := pool.Exec(ctx, `UPDATE auth_states SET expires_at = NOW() - INTERVAL '1 minute' WHERE state_hash = $1`, hash); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := s.ConsumeAuthState(ctx, state); !errors.Is(err, store.ErrAuthStateNotFound) {
		t.Fatalf("expired consume err = %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	u, err := s.UpsertUserByProvider(ctx, store.UpsertUserInput{
		Email: "c@example.com", Name: "Carol",
		Provider: "keycloak", ExternalID: "k-1",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	token, hash, err := store.NewSessionToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if err := s.InsertUserSession(ctx, hash, u.ID, store.SessionTTL, "Go-test/1.0"); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	view, err := s.GetUserSession(ctx, store.HashSessionToken(token))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if view.User.ID != u.ID {
		t.Fatalf("session user mismatch")
	}
	if view.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expiry in the past")
	}

	// Touch then delete.
	if err := s.TouchUserSession(ctx, hash); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := s.DeleteUserSession(ctx, hash); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetUserSession(ctx, hash); !errors.Is(err, store.ErrUserSessionNotFound) {
		t.Fatalf("after delete err = %v, want ErrUserSessionNotFound", err)
	}
}

func TestSession_ExpiredFilteredInQuery(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	u, _ := s.UpsertUserByProvider(ctx, store.UpsertUserInput{
		Email: "d@example.com", Name: "Dan",
		Provider: "oidc", ExternalID: "o-7",
	})
	_, hash, _ := store.NewSessionToken()
	_ = s.InsertUserSession(ctx, hash, u.ID, store.SessionTTL, "")

	// Age past expiry.
	if _, err := pool.Exec(ctx, `UPDATE user_sessions SET expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, hash); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := s.GetUserSession(ctx, hash); !errors.Is(err, store.ErrUserSessionNotFound) {
		t.Fatalf("expired session should 404: %v", err)
	}
}

func TestSession_DisabledUserRejected(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	u, _ := s.UpsertUserByProvider(ctx, store.UpsertUserInput{
		Email: "e@example.com", Name: "Eve",
		Provider: "github", ExternalID: "99",
	})
	_, hash, _ := store.NewSessionToken()
	_ = s.InsertUserSession(ctx, hash, u.ID, store.SessionTTL, "")

	if _, err := pool.Exec(ctx, `UPDATE users SET disabled_at = NOW() WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := s.GetUserSession(ctx, hash); !errors.Is(err, store.ErrUserDisabled) {
		t.Fatalf("err = %v, want ErrUserDisabled", err)
	}
	// And the session was swept on that failing lookup.
	var remaining int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_sessions WHERE id = $1`, hash).Scan(&remaining)
	if remaining != 0 {
		t.Fatalf("disabled user's session not cleaned up")
	}
}
