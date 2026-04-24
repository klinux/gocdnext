package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestLocalUser_CreateAndAuthenticate(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	u, err := s.CreateOrUpdateLocalUser(ctx, "Admin@Example.COM", "Admin", store.RoleAdmin, "hunter2pass")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Email is normalized to lower-case.
	if u.Email != "admin@example.com" {
		t.Fatalf("email = %q", u.Email)
	}
	if u.Provider != store.ProviderLocal || u.ExternalID != "admin@example.com" {
		t.Fatalf("provider/external_id wrong: %+v", u)
	}
	if u.Role != store.RoleAdmin {
		t.Fatalf("role = %q", u.Role)
	}

	// Happy path login.
	got, err := s.AuthenticateLocalUser(ctx, "admin@example.com", "hunter2pass")
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("user mismatch")
	}

	// Wrong password.
	_, err = s.AuthenticateLocalUser(ctx, "admin@example.com", "wrong")
	if !errors.Is(err, store.ErrLocalPasswordMismatch) {
		t.Fatalf("err = %v, want ErrLocalPasswordMismatch", err)
	}

	// Unknown email.
	_, err = s.AuthenticateLocalUser(ctx, "ghost@example.com", "anything-ok")
	if !errors.Is(err, store.ErrLocalUserNotFound) {
		t.Fatalf("err = %v, want ErrLocalUserNotFound", err)
	}
}

func TestLocalUser_UpdateRotatesPassword(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	if _, err := s.CreateOrUpdateLocalUser(ctx, "op@example.com", "Op", store.RoleMaintainer, "initial-pw"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateOrUpdateLocalUser(ctx, "op@example.com", "Op Renamed", store.RoleMaintainer, "rotated-pw"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := s.AuthenticateLocalUser(ctx, "op@example.com", "initial-pw"); !errors.Is(err, store.ErrLocalPasswordMismatch) {
		t.Fatalf("old password still works: %v", err)
	}
	u, err := s.AuthenticateLocalUser(ctx, "op@example.com", "rotated-pw")
	if err != nil {
		t.Fatalf("new password: %v", err)
	}
	if u.Name != "Op Renamed" {
		t.Fatalf("name = %q", u.Name)
	}
}

func TestLocalUser_DisabledRejected(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	u, _ := s.CreateOrUpdateLocalUser(ctx, "d@example.com", "D", store.RoleMaintainer, "disabledpw")
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled_at = NOW() WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	_, err := s.AuthenticateLocalUser(ctx, "d@example.com", "disabledpw")
	if !errors.Is(err, store.ErrUserDisabled) {
		t.Fatalf("err = %v, want ErrUserDisabled", err)
	}
}

func TestLocalUser_PasswordPolicy(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, err := s.CreateOrUpdateLocalUser(ctx, "x@example.com", "x", store.RoleMaintainer, "2short")
	if !errors.Is(err, store.ErrPasswordPolicy) {
		t.Fatalf("short password err = %v", err)
	}
}

func TestLocalUser_UpdatePasswordOnly(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	u, _ := s.CreateOrUpdateLocalUser(ctx, "p@example.com", "P", store.RoleAdmin, "oldpass99")
	if err := s.UpdateLocalUserPassword(ctx, u.ID, "newpass99"); err != nil {
		t.Fatalf("update: %v", err)
	}
	// Role must be unchanged — the password-only path should not
	// be a role-escalation vector.
	var role string
	_ = pool.QueryRow(ctx, `SELECT role FROM users WHERE id = $1`, u.ID).Scan(&role)
	if role != store.RoleAdmin {
		t.Fatalf("role drifted to %q", role)
	}
	if _, err := s.AuthenticateLocalUser(ctx, "p@example.com", "newpass99"); err != nil {
		t.Fatalf("auth with new pw: %v", err)
	}
}

func TestLocalUser_HasLocalUsers(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	has, err := s.HasLocalUsers(ctx)
	if err != nil || has {
		t.Fatalf("fresh DB: has=%v err=%v", has, err)
	}
	_, _ = s.CreateOrUpdateLocalUser(ctx, "h@example.com", "H", store.RoleAdmin, "goodpass1")
	has, _ = s.HasLocalUsers(ctx)
	if !has {
		t.Fatalf("expected has=true after insert")
	}
}
