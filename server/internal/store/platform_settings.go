// Package store: platform_settings is the home for runtime-mutable
// platform-wide config. Today the only setting that lives here is
// the artifact storage backend; future settings (SCM defaults,
// retention overrides, etc) reuse the same key/value row shape.
//
// Contract:
//   - `value` JSONB — non-secret config. The admin UI reads it
//     back unchanged on GET; it carries the round-trippable shape.
//   - `credentials_enc` BYTEA — AEAD-sealed plaintext from the
//     caller. Reads return the encrypted blob; the consumer
//     decrypts via the shared cipher (same one used for project
//     secrets, runner-profile secrets, auth providers).
//
// On the dispatch / boot path:
//
//	row, err := s.GetPlatformSetting(ctx, "artifacts.storage")
//	if err == ErrPlatformSettingNotFound { /* fall back to env */ }
//	creds := decrypt(cipher, row.CredentialsEnc)
//	use(row.Value, creds)
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrPlatformSettingNotFound is the canonical "no row" sentinel.
// Boot paths treat it as "use env config" rather than a fatal
// error — the platform_settings table is purely an override.
var ErrPlatformSettingNotFound = errors.New("store: platform setting not found")

// PlatformSetting is the read shape: `value` decoded into a
// generic map, encrypted blob untouched. Callers that need the
// secret half decrypt via DecryptCredentials.
type PlatformSetting struct {
	Key            string
	Value          map[string]any
	CredentialsEnc []byte
	UpdatedAt      time.Time
	UpdatedBy      *uuid.UUID
}

// PlatformSettingInput is the write shape. `Credentials` carries
// PLAINTEXT — Set sealed it with the cipher before persisting,
// so the row at rest only ever holds ciphertext. nil/empty
// credentials map means "no secrets for this setting" and stores
// NULL in the column.
type PlatformSettingInput struct {
	Key         string
	Value       map[string]any
	Credentials map[string]string
}

// GetPlatformSetting fetches a setting by key. Returns
// ErrPlatformSettingNotFound when no row matches; the fast path
// for the boot reader (no row → use env).
func (s *Store) GetPlatformSetting(ctx context.Context, key string) (PlatformSetting, error) {
	row, err := s.q.GetPlatformSetting(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlatformSetting{}, ErrPlatformSettingNotFound
	}
	if err != nil {
		return PlatformSetting{}, fmt.Errorf("store: get platform setting %q: %w", key, err)
	}
	out := PlatformSetting{
		Key:            row.Key,
		CredentialsEnc: row.CredentialsEnc,
		UpdatedAt:      row.UpdatedAt.Time,
	}
	if row.UpdatedBy.Valid {
		id := fromPgUUID(row.UpdatedBy)
		out.UpdatedBy = &id
	}
	if len(row.Value) > 0 {
		if err := json.Unmarshal(row.Value, &out.Value); err != nil {
			return PlatformSetting{}, fmt.Errorf("store: unmarshal platform setting %q: %w", key, err)
		}
	}
	return out, nil
}

// UpsertPlatformSetting writes a setting by key. Credentials are
// AEAD-sealed before hitting the column; an empty/nil credentials
// map persists as NULL (the row tracks "no secrets" separately
// from "secrets cleared to zero-length").
//
// `updatedBy` is the user id from the admin's session, stamped
// on the row for audit. uuid.Nil = system / migration / cli.
func (s *Store) UpsertPlatformSetting(ctx context.Context, cipher *crypto.Cipher, in PlatformSettingInput, updatedBy uuid.UUID) (PlatformSetting, error) {
	if in.Key == "" {
		return PlatformSetting{}, errors.New("store: platform setting key is required")
	}
	valueBytes, err := json.Marshal(in.Value)
	if err != nil {
		return PlatformSetting{}, fmt.Errorf("store: marshal platform setting value: %w", err)
	}
	var credsBlob []byte
	if len(in.Credentials) > 0 {
		if cipher == nil {
			return PlatformSetting{}, errors.New("store: platform setting credentials require cipher")
		}
		// Marshal first into a stable JSON object so on read we can
		// unmarshal back to a map[string]string. AEAD seal then
		// covers the whole structure.
		raw, err := json.Marshal(in.Credentials)
		if err != nil {
			return PlatformSetting{}, fmt.Errorf("store: marshal platform setting creds: %w", err)
		}
		credsBlob, err = cipher.Encrypt(raw)
		if err != nil {
			return PlatformSetting{}, fmt.Errorf("store: encrypt platform setting creds: %w", err)
		}
	}
	row, err := s.q.UpsertPlatformSetting(ctx, db.UpsertPlatformSettingParams{
		Key:            in.Key,
		Value:          valueBytes,
		CredentialsEnc: credsBlob,
		UpdatedBy:      pgtype.UUID{Bytes: updatedBy, Valid: updatedBy != uuid.Nil},
	})
	if err != nil {
		return PlatformSetting{}, fmt.Errorf("store: upsert platform setting %q: %w", in.Key, err)
	}
	out := PlatformSetting{
		Key:            row.Key,
		CredentialsEnc: row.CredentialsEnc,
		UpdatedAt:      row.UpdatedAt.Time,
	}
	if row.UpdatedBy.Valid {
		id := fromPgUUID(row.UpdatedBy)
		out.UpdatedBy = &id
	}
	if len(row.Value) > 0 {
		if err := json.Unmarshal(row.Value, &out.Value); err != nil {
			return PlatformSetting{}, fmt.Errorf("store: unmarshal platform setting %q: %w", in.Key, err)
		}
	}
	return out, nil
}

// DeletePlatformSetting removes a setting; idempotent (deleting a
// missing key is a no-op). Boot path then falls back to env.
func (s *Store) DeletePlatformSetting(ctx context.Context, key string) error {
	if err := s.q.DeletePlatformSetting(ctx, key); err != nil {
		return fmt.Errorf("store: delete platform setting %q: %w", key, err)
	}
	return nil
}

// DecryptPlatformCredentials unseals the credentials blob a Get
// returned. Returns nil when the row carries no creds.
func DecryptPlatformCredentials(cipher *crypto.Cipher, blob []byte) (map[string]string, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	if cipher == nil {
		return nil, errors.New("store: platform setting credentials present but cipher not configured")
	}
	plain, err := cipher.Decrypt(blob)
	if err != nil {
		return nil, fmt.Errorf("store: decrypt platform setting creds: %w", err)
	}
	out := map[string]string{}
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, fmt.Errorf("store: unmarshal platform setting creds: %w", err)
	}
	return out, nil
}

