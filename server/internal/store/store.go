// Package store is the typed persistence layer for gocdnext. It wraps the
// sqlc-generated queries and translates DB-shaped types into plain domain
// structs the rest of the server can consume without knowing about pgx or
// pgtype.
package store

import (
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// Sentinel errors callers can match with errors.Is.
var (
	ErrMaterialNotFound = errors.New("store: material not found")
)

// Store is the public entry point; construct once at server start.
type Store struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

// New wraps an already-configured pgx pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: db.New(pool)}
}

// FingerprintFor produces the canonical fingerprint for a git material. Thin
// alias over domain.GitFingerprint so the webhook handler (historical caller)
// doesn't need to switch packages; the actual logic lives in domain so the
// parser can reach it without importing store.
func FingerprintFor(cloneURL, branch string) string {
	return domain.GitFingerprint(cloneURL, branch)
}

// --- internal conversions between pgtype and domain types ---

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func fromPgUUID(id pgtype.UUID) uuid.UUID {
	if !id.Valid {
		return uuid.Nil
	}
	return uuid.UUID(id.Bytes)
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func stringValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
