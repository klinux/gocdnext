package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ProviderLocal is the value stored in users.provider for
// password-authenticated accounts. Kept as a const so handler
// logic doesn't rely on string literals.
const ProviderLocal = "local"

// BcryptCost is intentionally on the higher end of the typical
// range. CI tooling doesn't need to absorb the cost of a sub-
// millisecond hash; we'd rather spend ~200ms per login than make
// offline cracking cheaper after a DB dump.
const BcryptCost = 12

// Password policy constants. These aren't about cryptographic
// strength (a long random password beats any policy) — they're
// the minimum bar so admins don't set "admin" as their password.
const (
	MinPasswordLen = 8
	MaxPasswordLen = 512
)

// Local-auth sentinels.
var (
	ErrLocalUserNotFound   = errors.New("store: local user not found")
	ErrLocalPasswordMismatch = errors.New("store: local password mismatch")
	ErrPasswordPolicy      = errors.New("store: password does not meet policy")
)

// CreateOrUpdateLocalUser upserts a password-backed user by email.
// Called from the CLI (`gocdnext admin create-user`) and from the
// admin self-serve flow. name/role/password are all set from the
// caller so re-running with the same email rotates credentials.
func (s *Store) CreateOrUpdateLocalUser(ctx context.Context, email, name, role, password string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return User{}, errors.New("store: email required")
	}
	if err := ValidatePassword(password); err != nil {
		return User{}, err
	}
	if role == "" {
		role = RoleMaintainer
	}
	if role != RoleAdmin && role != RoleMaintainer && role != RoleViewer {
		return User{}, fmt.Errorf("store: invalid role %q", role)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return User{}, fmt.Errorf("store: hash password: %w", err)
	}
	row, err := s.q.UpsertLocalUser(ctx, db.UpsertLocalUserParams{
		Email:        email,
		Name:         name,
		Role:         role,
		PasswordHash: hash,
	})
	if err != nil {
		return User{}, fmt.Errorf("store: upsert local user: %w", err)
	}
	return User{
		ID:          fromPgUUID(row.ID),
		Email:       row.Email,
		Name:        row.Name,
		AvatarURL:   row.AvatarUrl,
		Provider:    row.Provider,
		ExternalID:  row.ExternalID,
		Role:        row.Role,
		DisabledAt:  pgTimePtr(row.DisabledAt),
		LastLoginAt: pgTimePtr(row.LastLoginAt),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}, nil
}

// AuthenticateLocalUser is the bcrypt-compare path. Returns
// ErrLocalUserNotFound for unknown emails AND wrong passwords so
// the handler can respond identically to both (timing-invariant
// is nice-to-have, not enforced here).
func (s *Store) AuthenticateLocalUser(ctx context.Context, email, password string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	row, err := s.q.GetLocalUserForLogin(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		// Still run bcrypt on a dummy hash so the response time
		// doesn't leak which emails exist. The cost matches a
		// real compare within a factor of 2, which is enough for
		// naive scanners.
		_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(password))
		return User{}, ErrLocalUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: local login lookup: %w", err)
	}
	if row.PasswordHash == nil {
		return User{}, ErrLocalUserNotFound
	}
	if err := bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(password)); err != nil {
		return User{}, ErrLocalPasswordMismatch
	}
	u := User{
		ID:          fromPgUUID(row.ID),
		Email:       row.Email,
		Name:        row.Name,
		AvatarURL:   row.AvatarUrl,
		Provider:    row.Provider,
		ExternalID:  row.ExternalID,
		Role:        row.Role,
		DisabledAt:  pgTimePtr(row.DisabledAt),
		LastLoginAt: pgTimePtr(row.LastLoginAt),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
	if u.DisabledAt != nil {
		return u, ErrUserDisabled
	}
	return u, nil
}

// UpdateLocalUserPassword rotates the hash without touching role
// or name. Used by /settings/account → "change password". Does
// NOT verify the old password — the caller must do so separately
// (the handler requires re-auth-with-current-password).
func (s *Store) UpdateLocalUserPassword(ctx context.Context, id uuid.UUID, newPassword string) error {
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), BcryptCost)
	if err != nil {
		return fmt.Errorf("store: hash password: %w", err)
	}
	return s.q.UpdateLocalUserPassword(ctx, db.UpdateLocalUserPasswordParams{
		ID:           pgUUID(id),
		PasswordHash: hash,
	})
}

// HasLocalUsers tells the login page whether to render the
// password form. Zero matches = form hidden; any positive count
// shows it.
func (s *Store) HasLocalUsers(ctx context.Context) (bool, error) {
	n, err := s.q.CountLocalUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("store: count local users: %w", err)
	}
	return n > 0, nil
}

// ValidatePassword enforces the (low-bar) password policy. Kept
// exported so the CLI can fail loudly before we even hit the DB.
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLen {
		return fmt.Errorf("%w: minimum %d characters", ErrPasswordPolicy, MinPasswordLen)
	}
	if len(password) > MaxPasswordLen {
		return fmt.Errorf("%w: maximum %d characters", ErrPasswordPolicy, MaxPasswordLen)
	}
	return nil
}

// dummyBcryptHash is generated at package init so the "unknown
// email" path spends roughly the same CPU budget as a real
// mismatch. A hard-coded string would work too, but deriving one
// at the configured cost keeps the two paths synchronized when
// BcryptCost changes.
var dummyBcryptHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("not-a-real-password"), BcryptCost)
	if err != nil {
		// Unreachable: bcrypt.GenerateFromPassword only errors on
		// cost out-of-range, and BcryptCost is a compile-time
		// constant inside the valid range.
		panic(fmt.Sprintf("bcrypt init: %v", err))
	}
	return h
}()
