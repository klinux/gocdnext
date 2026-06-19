package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/db"
)

// SecretBackendsChannel is the LISTEN/NOTIFY channel a backend config write
// fires (inside its tx, commit-gated) so every replica's registry hot-reloads.
const SecretBackendsChannel = "secret_backends_changed"

// secretBackendKey maps a source token to its platform_settings key.
func secretBackendKey(source string) string { return "secrets." + source }

// validSecretBackendSource gates the three external backend kinds.
func validSecretBackendSource(source string) bool {
	switch source {
	case SecretSourceVault, SecretSourceGCP, SecretSourceAWS:
		return true
	}
	return false
}

// SecretBackendInput is a backend config write. Value is the non-secret JSONB
// (incl. the `enabled` flag); Credentials is plaintext to seal (vault
// secret_id/token). PreserveCreds keeps the stored blob untouched (metadata-
// only edit) — the credential is never round-tripped to the client.
type SecretBackendInput struct {
	Source        string
	Value         map[string]any
	Credentials   map[string]string
	PreserveCreds bool
	UpdatedBy     uuid.UUID
}

// GetSecretBackend reads a backend's config row. ErrPlatformSettingNotFound
// when none — the registry then falls back to the env baseline.
func (s *Store) GetSecretBackend(ctx context.Context, source string) (PlatformSetting, error) {
	if !validSecretBackendSource(source) {
		return PlatformSetting{}, fmt.Errorf("store: unknown secret backend source %q", source)
	}
	return s.GetPlatformSetting(ctx, secretBackendKey(source))
}

// SetSecretBackend upserts a backend's config and fires SecretBackendsChannel
// in the same tx (commit-gated hot-reload). Credentials are sealed with the
// cipher; PreserveCreds reuses the stored blob (no decrypt needed).
func (s *Store) SetSecretBackend(ctx context.Context, cipher *crypto.Cipher, in SecretBackendInput) error {
	if !validSecretBackendSource(in.Source) {
		return fmt.Errorf("store: unknown secret backend source %q", in.Source)
	}
	key := secretBackendKey(in.Source)
	valueBytes, err := json.Marshal(in.Value)
	if err != nil {
		return fmt.Errorf("store: marshal secret backend value: %w", err)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: set secret backend: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	var credsBlob []byte
	switch {
	case in.PreserveCreds:
		// Keep the stored ciphertext as-is (metadata-only edit).
		if existing, gerr := q.GetPlatformSetting(ctx, key); gerr == nil {
			credsBlob = existing.CredentialsEnc
		} else if !errors.Is(gerr, pgx.ErrNoRows) {
			return fmt.Errorf("store: set secret backend: read existing: %w", gerr)
		}
	case len(in.Credentials) > 0:
		if cipher == nil {
			return errors.New("store: secret backend credentials require cipher (set GOCDNEXT_SECRET_KEY)")
		}
		raw, merr := json.Marshal(in.Credentials)
		if merr != nil {
			return fmt.Errorf("store: marshal secret backend creds: %w", merr)
		}
		credsBlob, err = cipher.Encrypt(raw)
		if err != nil {
			return fmt.Errorf("store: encrypt secret backend creds: %w", err)
		}
	}

	if _, err := q.UpsertPlatformSetting(ctx, db.UpsertPlatformSettingParams{
		Key:            key,
		Value:          valueBytes,
		CredentialsEnc: credsBlob,
		UpdatedBy:      pgtype.UUID{Bytes: in.UpdatedBy, Valid: in.UpdatedBy != uuid.Nil},
	}); err != nil {
		return fmt.Errorf("store: upsert secret backend %q: %w", in.Source, err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, SecretBackendsChannel, in.Source); err != nil {
		return fmt.Errorf("store: set secret backend: notify: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: set secret backend: commit: %w", err)
	}
	return nil
}

// DeleteSecretBackend drops a backend's config (registry falls back to env) and
// fires the change notice in the same tx.
func (s *Store) DeleteSecretBackend(ctx context.Context, source string) error {
	if !validSecretBackendSource(source) {
		return fmt.Errorf("store: unknown secret backend source %q", source)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: delete secret backend: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	if err := q.DeletePlatformSetting(ctx, secretBackendKey(source)); err != nil {
		return fmt.Errorf("store: delete secret backend %q: %w", source, err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, SecretBackendsChannel, source); err != nil {
		return fmt.Errorf("store: delete secret backend: notify: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: delete secret backend: commit: %w", err)
	}
	return nil
}
