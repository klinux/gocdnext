// Package admin implements CLI-side admin operations that bypass
// the control-plane HTTP API. Today that's local-user provisioning
// — needed at bootstrap time when there's no admin to log in with.
// Opens a direct pgx connection to GOCDNEXT_DATABASE_URL; the
// server does not have to be running.
package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// Roles match the CHECK constraint on users.role. Duplicated from
// the server package on purpose — the CLI is a separate Go module
// and can't reach server/internal/*.
const (
	RoleAdmin  = "admin"
	RoleUser   = "user"
	RoleViewer = "viewer"
)

// Password policy. Mirrored from the server side; see
// server/internal/store/local_users.go. Change in both places.
const (
	minPasswordLen = 8
	maxPasswordLen = 512
	bcryptCost     = 12
)

// ValidateRole returns an error for any role outside the enum.
func ValidateRole(role string) error {
	switch role {
	case RoleAdmin, RoleUser, RoleViewer:
		return nil
	default:
		return fmt.Errorf("invalid role %q (expected admin|user|viewer)", role)
	}
}

// ValidatePassword applies the length policy. No complexity rules —
// length beats any policy, and we'd rather admins reach for a
// password manager.
func ValidatePassword(password string) error {
	if len(password) < minPasswordLen {
		return fmt.Errorf("password too short (min %d characters)", minPasswordLen)
	}
	if len(password) > maxPasswordLen {
		return fmt.Errorf("password too long (max %d characters)", maxPasswordLen)
	}
	return nil
}

// CreateOrUpdateLocalUser upserts a local user by email. The
// caller must have already prompted for + validated the password.
// Name defaults to the local-part of the email when empty.
func CreateOrUpdateLocalUser(ctx context.Context, databaseURL, email, name, role, password string) (created bool, err error) {
	if err := ValidateRole(role); err != nil {
		return false, err
	}
	if err := ValidatePassword(password); err != nil {
		return false, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return false, errors.New("valid email required")
	}
	if name == "" {
		name = localPart(email)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return false, fmt.Errorf("bcrypt: %w", err)
	}

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return false, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	// Two-phase: detect whether the row exists so we can report
	// "created" vs "rotated" accurately. A single UPSERT would work
	// but couldn't tell the user what just happened.
	var existing bool
	if err := conn.QueryRow(ctx,
		`SELECT true FROM users WHERE provider = 'local' AND external_id = $1`, email,
	).Scan(&existing); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("lookup: %w", err)
	}

	_, err = conn.Exec(ctx, `
		INSERT INTO users (email, name, avatar_url, provider, external_id, role, password_hash)
		VALUES ($1, $2, '', 'local', $1, $3, $4)
		ON CONFLICT (provider, external_id) DO UPDATE SET
		    name          = EXCLUDED.name,
		    role          = EXCLUDED.role,
		    password_hash = EXCLUDED.password_hash,
		    updated_at    = NOW()
	`, email, name, role, hash)
	if err != nil {
		return false, fmt.Errorf("upsert: %w", err)
	}
	return !existing, nil
}

// ResetPassword rotates only the password_hash for an existing
// local user. Errors with a distinct message when the user
// doesn't exist (admin should use create-user for that).
func ResetPassword(ctx context.Context, databaseURL, email, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	email = strings.ToLower(strings.TrimSpace(email))

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	tag, err := conn.Exec(ctx, `
		UPDATE users
		SET password_hash = $2, updated_at = NOW()
		WHERE provider = 'local' AND external_id = $1
	`, email, hash)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no local user for %s (use `admin create-user` instead)", email)
	}
	return nil
}

func localPart(email string) string {
	at := strings.IndexByte(email, '@')
	if at < 0 {
		return email
	}
	return email[:at]
}
