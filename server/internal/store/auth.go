package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// Auth-layer errors. Handlers map these to HTTP status codes.
var (
	ErrAuthStateNotFound = errors.New("store: auth state not found or expired")
	ErrUserSessionNotFound = errors.New("store: user session not found or expired")
	ErrUserDisabled      = errors.New("store: user is disabled")
)

// Role constants so callers and migrations agree on the enum.
const (
	RoleAdmin  = "admin"
	RoleUser   = "user"
	RoleViewer = "viewer"
)

// AuthStateTTL is how long an OAuth `state` parameter stays valid
// between /auth/login redirect and /auth/callback. Users who dawdle
// longer than this on the IdP page get bounced back with a "state
// expired" error rather than silently matching a stale state row.
const AuthStateTTL = 10 * time.Minute

// SessionTTL is the default browser session duration. Fresh at login,
// stamped into the session row's expires_at.
const SessionTTL = 24 * time.Hour

// User is the domain projection the rest of the server consumes.
type User struct {
	ID          uuid.UUID  `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	AvatarURL   string     `json:"avatar_url,omitempty"`
	Provider    string     `json:"provider"`
	ExternalID  string     `json:"external_id"`
	Role        string     `json:"role"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// UpsertUserInput matches the shape of claims we pull off an IdP.
// Role is set only on insert (ON CONFLICT DO UPDATE skips role).
type UpsertUserInput struct {
	Email       string
	Name        string
	AvatarURL   string
	Provider    string
	ExternalID  string
	InitialRole string
}

// UpsertUserByProvider creates or refreshes a user row on login.
// Disabled users raise ErrUserDisabled so the caller can 403 without
// minting a session.
func (s *Store) UpsertUserByProvider(ctx context.Context, in UpsertUserInput) (User, error) {
	role := in.InitialRole
	if role == "" {
		role = RoleUser
	}
	row, err := s.q.UpsertUserByProvider(ctx, db.UpsertUserByProviderParams{
		Email:      in.Email,
		Name:       in.Name,
		AvatarUrl:  in.AvatarURL,
		Provider:   in.Provider,
		ExternalID: in.ExternalID,
		Role:       role,
	})
	if err != nil {
		return User{}, fmt.Errorf("store: upsert user: %w", err)
	}
	u := userFromDBRow(row)
	if u.DisabledAt != nil {
		return u, ErrUserDisabled
	}
	return u, nil
}

// GetUser returns a user row by UUID. 404 maps to pgx.ErrNoRows;
// callers should not bubble that to the client.
func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	row, err := s.q.GetUserByID(ctx, pgUUID(id))
	if err != nil {
		return User{}, err
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

// NewAuthState generates a hex-encoded random state token + its
// paired DB row. Caller stuffs the returned token into the OAuth
// redirect URL; the row lets us validate + look up redirect_to at
// callback time. nonce is passed through (OIDC needs it) or left
// empty for pure-OAuth providers like GitHub.
func (s *Store) NewAuthState(ctx context.Context, provider, redirectTo, nonce string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("store: auth state rng: %w", err)
	}
	state := hex.EncodeToString(raw)
	hash := sha256.Sum256([]byte(state))
	if err := s.q.InsertAuthState(ctx, db.InsertAuthStateParams{
		StateHash:  hash[:],
		Provider:   provider,
		RedirectTo: redirectTo,
		Nonce:      nonce,
		ExpiresAt:  timestampOrNull(time.Now().Add(AuthStateTTL)),
	}); err != nil {
		return "", fmt.Errorf("store: insert auth state: %w", err)
	}
	return state, nil
}

// AuthStateConsumeResult is what ConsumeAuthState hands back so the
// callback handler can validate `provider` and forward the user to
// `redirect_to`.
type AuthStateConsumeResult struct {
	Provider   string
	RedirectTo string
	Nonce      string
}

// ConsumeAuthState deletes the row and returns the metadata if the
// token is valid + unexpired. A second consume on the same token is
// guaranteed to fail (single-use DELETE ... RETURNING).
func (s *Store) ConsumeAuthState(ctx context.Context, state string) (AuthStateConsumeResult, error) {
	hash := sha256.Sum256([]byte(state))
	row, err := s.q.ConsumeAuthState(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthStateConsumeResult{}, ErrAuthStateNotFound
	}
	if err != nil {
		return AuthStateConsumeResult{}, fmt.Errorf("store: consume auth state: %w", err)
	}
	return AuthStateConsumeResult{
		Provider:   row.Provider,
		RedirectTo: row.RedirectTo,
		Nonce:      row.Nonce,
	}, nil
}

// NewSessionToken returns (cookie_value, cookie_hash). Store the
// hash in the DB; hand the plaintext to the browser. 32 bytes
// (hex-encoded 64 chars) is comfortably above what a brute-forcer
// can enumerate and fits in a single Set-Cookie just fine.
func NewSessionToken() (plain string, hash []byte, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("store: session rng: %w", err)
	}
	plain = hex.EncodeToString(raw)
	h := sha256.Sum256([]byte(plain))
	return plain, h[:], nil
}

// HashSessionToken lets the middleware reuse the hash scheme without
// exporting crypto/sha256 at every call site.
func HashSessionToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// InsertUserSession creates a session row. Expiry is NOW() + ttl.
func (s *Store) InsertUserSession(ctx context.Context, hash []byte, userID uuid.UUID, ttl time.Duration, userAgent string) error {
	if ttl <= 0 {
		ttl = SessionTTL
	}
	return s.q.InsertUserSession(ctx, db.InsertUserSessionParams{
		ID:        hash,
		UserID:    pgUUID(userID),
		ExpiresAt: timestampOrNull(time.Now().Add(ttl)),
		UserAgent: userAgent,
	})
}

// SessionView is what the middleware gets back from a successful
// GetUserSession: the full user + session expiry.
type SessionView struct {
	User       User
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// GetUserSession validates a session cookie. Expired rows are
// filtered in the query — a miss here is either "never existed" or
// "aged out"; either way the cookie is no longer valid.
func (s *Store) GetUserSession(ctx context.Context, hash []byte) (SessionView, error) {
	row, err := s.q.GetUserSession(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionView{}, ErrUserSessionNotFound
	}
	if err != nil {
		return SessionView{}, fmt.Errorf("store: get session: %w", err)
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
		// Best-effort clean-up so a reconnect doesn't keep handing us
		// the same stale session.
		_ = s.q.DeleteUserSession(ctx, hash)
		return SessionView{}, ErrUserDisabled
	}
	return SessionView{
		User:       u,
		ExpiresAt:  row.ExpiresAt.Time,
		LastSeenAt: row.LastSeenAt.Time,
	}, nil
}

// TouchUserSession bumps last_seen_at. Safe to call from the
// middleware's background goroutine.
func (s *Store) TouchUserSession(ctx context.Context, hash []byte) error {
	return s.q.TouchUserSession(ctx, hash)
}

// DeleteUserSession is the store-side "logout". Idempotent.
func (s *Store) DeleteUserSession(ctx context.Context, hash []byte) error {
	return s.q.DeleteUserSession(ctx, hash)
}

// SweepAuthState + SweepUserSessions: hook points for the retention
// sweeper so expired rows don't accumulate.
func (s *Store) SweepAuthStates(ctx context.Context) error {
	return s.q.DeleteExpiredAuthStates(ctx)
}

func (s *Store) SweepUserSessions(ctx context.Context) error {
	return s.q.DeleteExpiredUserSessions(ctx)
}

// --- locals ---

func userFromDBRow(row db.User) User {
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
	}
}
