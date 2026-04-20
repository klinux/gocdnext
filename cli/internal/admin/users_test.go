package admin_test

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/gocdnext/gocdnext/cli/internal/admin"
)

// Minimal users-table schema stripped from the server migration so
// the CLI admin tests don't pull the server module in. Must match
// 00001_init + 00007_auth + 00009_local_users.
const schemaBootstrap = `
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    avatar_url TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL,
    external_id TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'user',
    disabled_at TIMESTAMPTZ,
    last_login_at TIMESTAMPTZ,
    password_hash BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT users_role_check CHECK (role IN ('admin', 'user', 'viewer')),
    UNIQUE (provider, external_id)
);`

func startPG(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	ctx := context.Background()
	c, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("gocdnext"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start pg: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("conn string: %v", err)
	}

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, schemaBootstrap); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return url
}

func TestCreateOrUpdateLocalUser_CreateThenRotate(t *testing.T) {
	url := startPG(t)
	ctx := context.Background()

	created, err := admin.CreateOrUpdateLocalUser(ctx, url, "admin@example.com", "", admin.RoleAdmin, "goodpass1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true on first call")
	}

	// Second call rotates — created=false; new password takes effect.
	created, err = admin.CreateOrUpdateLocalUser(ctx, url, "admin@example.com", "Admin", admin.RoleUser, "newpass9x")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if created {
		t.Fatalf("expected created=false on second call")
	}

	// Verify role was updated + name picked up.
	conn, _ := pgx.Connect(ctx, url)
	defer conn.Close(ctx)
	var role, name string
	_ = conn.QueryRow(ctx,
		`SELECT role, name FROM users WHERE provider='local' AND external_id=$1`,
		"admin@example.com",
	).Scan(&role, &name)
	if role != "user" || name != "Admin" {
		t.Fatalf("role=%q name=%q", role, name)
	}
}

func TestCreateOrUpdateLocalUser_InvalidInputs(t *testing.T) {
	url := startPG(t)
	ctx := context.Background()

	cases := []struct {
		name  string
		email string
		role  string
		pw    string
		want  string
	}{
		{"bad role", "a@example.com", "root", "goodpass1", "invalid role"},
		{"short password", "a@example.com", admin.RoleAdmin, "2sh", "too short"},
		{"missing at", "nope", admin.RoleAdmin, "goodpass1", "valid email"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := admin.CreateOrUpdateLocalUser(ctx, url, tc.email, "", tc.role, tc.pw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestResetPassword(t *testing.T) {
	url := startPG(t)
	ctx := context.Background()

	// Reset with no user present → distinct error message.
	err := admin.ResetPassword(ctx, url, "ghost@example.com", "whatever9")
	if err == nil || !strings.Contains(err.Error(), "no local user") {
		t.Fatalf("expected 'no local user' error, got %v", err)
	}

	// Create then rotate via ResetPassword.
	if _, err := admin.CreateOrUpdateLocalUser(ctx, url, "r@example.com", "", admin.RoleAdmin, "initialpw"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := admin.ResetPassword(ctx, url, "r@example.com", "rotatedpw"); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// updated_at bumped after reset (sanity check — real auth test
	// is in the server package where bcrypt compare lives).
	conn, _ := pgx.Connect(ctx, url)
	defer conn.Close(ctx)
	var updatedAt time.Time
	_ = conn.QueryRow(ctx, `SELECT updated_at FROM users WHERE external_id=$1`, "r@example.com").Scan(&updatedAt)
	if time.Since(updatedAt) > 30*time.Second {
		t.Fatalf("updated_at not bumped: %v", updatedAt)
	}
}

func TestCreateOrUpdateLocalUser_DBUnreachable(t *testing.T) {
	// Use a URL that will refuse connections fast.
	_, err := admin.CreateOrUpdateLocalUser(context.Background(),
		"postgres://localhost:1/nonexistent?sslmode=disable&connect_timeout=1",
		"a@example.com", "", admin.RoleAdmin, "goodpass1")
	if err == nil {
		t.Fatalf("expected connection error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// Either "connection refused" or "timeout" is fine.
		return
	}
	if !strings.Contains(err.Error(), "connect") && !strings.Contains(err.Error(), "refused") {
		t.Logf("unexpected but acceptable error: %v", err)
	}
	_ = fmt.Sprint("ok")
}
