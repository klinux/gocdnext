// Package store is the typed persistence layer for gocdnext. It wraps the
// sqlc-generated queries and translates DB-shaped types into plain domain
// structs the rest of the server can consume without knowing about pgx or
// pgtype.
package store

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/logarchive"
)

// Sentinel errors callers can match with errors.Is.
var (
	ErrMaterialNotFound = errors.New("store: material not found")
)

// Store is the public entry point; construct once at server start.
type Store struct {
	pool       *pgxpool.Pool
	q          *db.Queries
	authCipher *crypto.Cipher // optional; set via SetAuthCipher
	// logArchiveSrc is set when log archiving is wired (the artifact
	// backend serves the underlying bytes). nil = no fallback; the
	// read path stays on log_lines exclusively.
	logArchiveSrc   LogArchiveSource
	logArchiveCache *logarchive.LineCache // optional; nil = always fetch
}

// LogArchiveSource is the narrow byte-reader interface the read
// fallback needs. Implemented by artifacts.Store (and any test
// fake). Lifted to an interface so this package doesn't depend on
// internal/artifacts.
type LogArchiveSource interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// New wraps an already-configured pgx pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: db.New(pool)}
}

// WithLogArchiveSource enables the archived-logs read fallback.
// When set, reads against a job whose logs_archive_uri is
// populated stream bytes through this source instead of querying
// log_lines (which by then have been deleted).
func (s *Store) WithLogArchiveSource(src LogArchiveSource) *Store {
	s.logArchiveSrc = src
	return s
}

// WithLogArchiveCache attaches an LRU cache that memoises decoded
// archives. Without it every read of an archived job re-fetches +
// re-parses the gzip — a typical run-detail reload makes that
// hurt visibly on archives spanning thousands of lines. Nil-safe:
// callers that don't care can skip this setter.
func (s *Store) WithLogArchiveCache(c *logarchive.LineCache) *Store {
	s.logArchiveCache = c
	return s
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

func pgTimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
