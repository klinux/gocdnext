package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrAgentNotFound is returned by FindAgentByName when the agent row is
// absent. Callers typically map this to the gRPC NotFound status.
var ErrAgentNotFound = errors.New("store: agent not found")

// Agent mirrors the columns we need from the agents row.
type Agent struct {
	ID           uuid.UUID
	Name         string
	TokenHash    string
	Version      string
	OS           string
	Arch         string
	Tags         []string
	Capacity     int32
	Status       string
	LastSeenAt   time.Time
	RegisteredAt time.Time
}

// RegisterUpdate carries the fields an agent refreshes on each Register call.
// Empty strings / nil slices are stored as-is (not treated as "don't change").
type RegisterUpdate struct {
	Version  string
	OS       string
	Arch     string
	Tags     []string
	Capacity int32
}

// HashToken returns a deterministic hex-encoded SHA-256 of the plain token.
// Tokens are expected to be high-entropy random strings; bcrypt's work-factor
// only pays off for low-entropy human passwords.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// VerifyToken constant-time compares plain against the stored hash. An empty
// hash is always rejected (so an unpopulated column cannot authenticate).
func VerifyToken(plain, hash string) bool {
	if hash == "" {
		return false
	}
	expected, err := hex.DecodeString(hash)
	if err != nil {
		return false
	}
	got := sha256.Sum256([]byte(plain))
	return hmac.Equal(got[:], expected)
}

// FindAgentByName returns ErrAgentNotFound when no row matches. Other errors
// are wrapped verbatim.
func (s *Store) FindAgentByName(ctx context.Context, name string) (Agent, error) {
	row, err := s.q.FindAgentByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Agent{}, ErrAgentNotFound
		}
		return Agent{}, fmt.Errorf("store: find agent: %w", err)
	}
	return agentFromDB(row), nil
}

// MarkAgentOnline persists the metadata coming from a Register call and flips
// the agent to status='online', last_seen_at=NOW().
func (s *Store) MarkAgentOnline(ctx context.Context, id uuid.UUID, upd RegisterUpdate) error {
	err := s.q.UpdateAgentOnRegister(ctx, db.UpdateAgentOnRegisterParams{
		ID:       pgUUID(id),
		Version:  nullableString(upd.Version),
		Os:       nullableString(upd.OS),
		Arch:     nullableString(upd.Arch),
		Tags:     emptyIfNil(upd.Tags),
		Capacity: upd.Capacity,
	})
	if err != nil {
		return fmt.Errorf("store: mark agent online: %w", err)
	}
	return nil
}

// MarkAgentOffline flips status without touching metadata. Called when the
// Connect stream closes.
func (s *Store) MarkAgentOffline(ctx context.Context, id uuid.UUID) error {
	if err := s.q.MarkAgentOffline(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("store: mark agent offline: %w", err)
	}
	return nil
}

func agentFromDB(row db.Agent) Agent {
	return Agent{
		ID:           fromPgUUID(row.ID),
		Name:         row.Name,
		TokenHash:    row.TokenHash,
		Version:      stringValue(row.Version),
		OS:           stringValue(row.Os),
		Arch:         stringValue(row.Arch),
		Tags:         row.Tags,
		Capacity:     row.Capacity,
		Status:       row.Status,
		LastSeenAt:   row.LastSeenAt.Time,
		RegisteredAt: row.RegisteredAt.Time,
	}
}

func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
