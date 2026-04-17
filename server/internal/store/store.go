// Package store is the typed persistence layer for gocdnext. It wraps the
// sqlc-generated queries and translates DB-shaped types into plain domain
// structs the rest of the server can consume without knowing about pgx or
// pgtype.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/db"
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

// FingerprintFor produces the canonical fingerprint for a git material.
// Normalization: lowercase host, strip ".git" suffix, strip trailing slash.
// This must stay deterministic — the same (clone_url, branch) pair from a
// pipeline config and from a webhook payload has to collide on purpose.
func FingerprintFor(cloneURL, branch string) string {
	u := normalizeGitURL(cloneURL)
	h := sha256.Sum256([]byte(u + "\x00" + branch))
	return hex.EncodeToString(h[:])
}

func normalizeGitURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")
	// lowercase the host part only; path is case-sensitive on some forges.
	if i := strings.Index(s, "://"); i != -1 {
		rest := s[i+3:]
		if j := strings.Index(rest, "/"); j != -1 {
			host := strings.ToLower(rest[:j])
			s = s[:i+3] + host + rest[j:]
		} else {
			s = s[:i+3] + strings.ToLower(rest)
		}
	}
	return s
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
