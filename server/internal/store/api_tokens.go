package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// APIToken is the read-side shape of an api_tokens row, with the
// XOR'd FK collapsed into a (Subject, SubjectID) pair so callers
// don't have to look at two nullable columns.
type APIToken struct {
	ID          uuid.UUID
	Subject     TokenSubject
	SubjectID   uuid.UUID
	Name        string
	Prefix      string
	ExpiresAt   *time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
	CreatedAt   time.Time
}

// TokenSubject narrows the API token's identity boundary.
type TokenSubject string

const (
	TokenSubjectUser           TokenSubject = "user"
	TokenSubjectServiceAccount TokenSubject = "service_account"
)

// ErrAPITokenNotFound is returned when the bearer middleware's
// hash lookup finds no live token (revoked, expired, or never
// existed).
var ErrAPITokenNotFound = errors.New("store: api token not found")

// CreateUserAPIToken inserts a new token row keyed to a user.
// Caller computed the hash from the plaintext body via
// `apitoken.Hash` — this layer just persists.
func (s *Store) CreateUserAPIToken(ctx context.Context, userID uuid.UUID, name, hash, prefix string, expiresAt *time.Time) (APIToken, error) {
	row, err := s.q.InsertAPIToken(ctx, db.InsertAPITokenParams{
		UserID:           pgtype.UUID{Bytes: userID, Valid: true},
		ServiceAccountID: pgtype.UUID{Valid: false},
		Name:             name,
		Hash:             hash,
		Prefix:           prefix,
		ExpiresAt:        nullableTimestamp(expiresAt),
	})
	if err != nil {
		return APIToken{}, fmt.Errorf("store: insert api token: %w", err)
	}
	return apiTokenFromInsertRow(row), nil
}

// CreateSAAPIToken inserts a new token keyed to a service
// account.
func (s *Store) CreateSAAPIToken(ctx context.Context, saID uuid.UUID, name, hash, prefix string, expiresAt *time.Time) (APIToken, error) {
	row, err := s.q.InsertAPIToken(ctx, db.InsertAPITokenParams{
		UserID:           pgtype.UUID{Valid: false},
		ServiceAccountID: pgtype.UUID{Bytes: saID, Valid: true},
		Name:             name,
		Hash:             hash,
		Prefix:           prefix,
		ExpiresAt:        nullableTimestamp(expiresAt),
	})
	if err != nil {
		return APIToken{}, fmt.Errorf("store: insert api token: %w", err)
	}
	return apiTokenFromInsertRow(row), nil
}

// LookupAPITokenByHash is the bearer middleware's hot path. The
// query already filters revoked/expired so an active hit means
// "this token is good, route the request as the holder".
func (s *Store) LookupAPITokenByHash(ctx context.Context, hash string) (APIToken, error) {
	row, err := s.q.GetAPITokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return APIToken{}, ErrAPITokenNotFound
		}
		return APIToken{}, fmt.Errorf("store: lookup api token: %w", err)
	}
	return apiTokenFromLookupRow(row), nil
}

// TouchAPITokenLastUsed updates the last_used_at stamp. Called
// post-validation; failure is non-fatal (the caller logs +
// continues).
func (s *Store) TouchAPITokenLastUsed(ctx context.Context, id uuid.UUID) error {
	if err := s.q.TouchAPITokenLastUsed(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("store: touch api token: %w", err)
	}
	return nil
}

// ListAPITokensForUser returns every token the user owns, newest
// first. Used by /settings/api-tokens.
func (s *Store) ListAPITokensForUser(ctx context.Context, userID uuid.UUID) ([]APIToken, error) {
	rows, err := s.q.ListAPITokensByUser(ctx, pgtype.UUID{Bytes: userID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("store: list user tokens: %w", err)
	}
	out := make([]APIToken, 0, len(rows))
	for _, r := range rows {
		out = append(out, APIToken{
			ID:         fromPgUUID(r.ID),
			Subject:    TokenSubjectUser,
			SubjectID:  userID,
			Name:       r.Name,
			Prefix:     r.Prefix,
			ExpiresAt:  pgTimePtr(r.ExpiresAt),
			LastUsedAt: pgTimePtr(r.LastUsedAt),
			RevokedAt:  pgTimePtr(r.RevokedAt),
			CreatedAt:  r.CreatedAt.Time,
		})
	}
	return out, nil
}

// ListAPITokensForSA returns every token a service account owns.
func (s *Store) ListAPITokensForSA(ctx context.Context, saID uuid.UUID) ([]APIToken, error) {
	rows, err := s.q.ListAPITokensByServiceAccount(ctx, pgtype.UUID{Bytes: saID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("store: list sa tokens: %w", err)
	}
	out := make([]APIToken, 0, len(rows))
	for _, r := range rows {
		out = append(out, APIToken{
			ID:         fromPgUUID(r.ID),
			Subject:    TokenSubjectServiceAccount,
			SubjectID:  saID,
			Name:       r.Name,
			Prefix:     r.Prefix,
			ExpiresAt:  pgTimePtr(r.ExpiresAt),
			LastUsedAt: pgTimePtr(r.LastUsedAt),
			RevokedAt:  pgTimePtr(r.RevokedAt),
			CreatedAt:  r.CreatedAt.Time,
		})
	}
	return out, nil
}

// RevokeUserAPIToken revokes a token after verifying it belongs
// to the calling user. ErrAPITokenNotFound when the row doesn't
// exist OR belongs to someone else — handler maps both to 404 so
// we don't leak ID existence.
func (s *Store) RevokeUserAPIToken(ctx context.Context, tokenID, userID uuid.UUID) error {
	row, err := s.q.GetAPITokenForUserOwner(ctx, db.GetAPITokenForUserOwnerParams{
		ID:     pgUUID(tokenID),
		UserID: pgtype.UUID{Bytes: userID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAPITokenNotFound
		}
		return fmt.Errorf("store: lookup token for revoke: %w", err)
	}
	if row.RevokedAt.Valid {
		return nil // already revoked, idempotent
	}
	if err := s.q.RevokeAPIToken(ctx, pgUUID(tokenID)); err != nil {
		return fmt.Errorf("store: revoke token: %w", err)
	}
	return nil
}

// RevokeSAAPIToken is the SA equivalent — admin path. Same
// not-found semantics.
func (s *Store) RevokeSAAPIToken(ctx context.Context, tokenID, saID uuid.UUID) error {
	row, err := s.q.GetAPITokenForSAOwner(ctx, db.GetAPITokenForSAOwnerParams{
		ID:               pgUUID(tokenID),
		ServiceAccountID: pgtype.UUID{Bytes: saID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAPITokenNotFound
		}
		return fmt.Errorf("store: lookup sa token for revoke: %w", err)
	}
	if row.RevokedAt.Valid {
		return nil
	}
	if err := s.q.RevokeAPIToken(ctx, pgUUID(tokenID)); err != nil {
		return fmt.Errorf("store: revoke sa token: %w", err)
	}
	return nil
}

func nullableTimestamp(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{Valid: false}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func apiTokenFromInsertRow(r db.InsertAPITokenRow) APIToken {
	out := APIToken{
		ID:         fromPgUUID(r.ID),
		Name:       r.Name,
		Prefix:     r.Prefix,
		ExpiresAt:  pgTimePtr(r.ExpiresAt),
		LastUsedAt: pgTimePtr(r.LastUsedAt),
		RevokedAt:  pgTimePtr(r.RevokedAt),
		CreatedAt:  r.CreatedAt.Time,
	}
	if r.UserID.Valid {
		out.Subject = TokenSubjectUser
		out.SubjectID = fromPgUUID(r.UserID)
	} else {
		out.Subject = TokenSubjectServiceAccount
		out.SubjectID = fromPgUUID(r.ServiceAccountID)
	}
	return out
}

func apiTokenFromLookupRow(r db.GetAPITokenByHashRow) APIToken {
	out := APIToken{
		ID:         fromPgUUID(r.ID),
		Name:       r.Name,
		Prefix:     r.Prefix,
		ExpiresAt:  pgTimePtr(r.ExpiresAt),
		LastUsedAt: pgTimePtr(r.LastUsedAt),
		RevokedAt:  pgTimePtr(r.RevokedAt),
		CreatedAt:  r.CreatedAt.Time,
	}
	if r.UserID.Valid {
		out.Subject = TokenSubjectUser
		out.SubjectID = fromPgUUID(r.UserID)
	} else {
		out.Subject = TokenSubjectServiceAccount
		out.SubjectID = fromPgUUID(r.ServiceAccountID)
	}
	return out
}
