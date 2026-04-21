package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrSecretNotFound signals DeleteSecret had nothing to remove.
var ErrSecretNotFound = errors.New("store: secret not found")

// SecretName pattern: letters, digits, underscore, up to 64 chars, must start
// with a letter. Keeps secret names safe to turn straight into env var names
// (runner injects them as FOO=value via the assignment env map).
var secretNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// Secret is the list-side view: no value. Exposed on the API so ops can audit
// "which secrets does this project declare" without being able to read them.
type Secret struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SecretSet is the write-side input. Plaintext lives in memory only long
// enough for Encrypt to consume it; nothing on the DB side ever sees it.
type SecretSet struct {
	ProjectID uuid.UUID
	Name      string
	Value     []byte
}

// SetSecret encrypts the plaintext and upserts by (project_id, name).
// Returns whether the row was freshly created.
func (s *Store) SetSecret(ctx context.Context, cipher *crypto.Cipher, in SecretSet) (bool, error) {
	if cipher == nil {
		return false, errors.New("store: secrets: cipher not configured")
	}
	if err := ValidateSecretName(in.Name); err != nil {
		return false, err
	}
	enc, err := cipher.Encrypt(in.Value)
	if err != nil {
		return false, fmt.Errorf("store: encrypt secret: %w", err)
	}
	row, err := s.q.UpsertSecret(ctx, db.UpsertSecretParams{
		ProjectID: pgUUID(in.ProjectID),
		Name:      in.Name,
		ValueEnc:  enc,
	})
	if err != nil {
		return false, fmt.Errorf("store: upsert secret: %w", err)
	}
	return row.Created, nil
}

// ListSecrets returns names + timestamps. Never returns the value.
func (s *Store) ListSecrets(ctx context.Context, projectID uuid.UUID) ([]Secret, error) {
	rows, err := s.q.ListSecretsByProject(ctx, pgUUID(projectID))
	if err != nil {
		return nil, fmt.Errorf("store: list secrets: %w", err)
	}
	out := make([]Secret, 0, len(rows))
	for _, r := range rows {
		out = append(out, Secret{
			Name:      r.Name,
			CreatedAt: r.CreatedAt.Time,
			UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// DeleteSecret removes a secret by name. Returns ErrSecretNotFound when no
// row matched so the HTTP layer can map to 404.
func (s *Store) DeleteSecret(ctx context.Context, projectID uuid.UUID, name string) error {
	n, err := s.q.DeleteSecretByName(ctx, db.DeleteSecretByNameParams{
		ProjectID: pgUUID(projectID),
		Name:      name,
	})
	if err != nil {
		return fmt.Errorf("store: delete secret: %w", err)
	}
	if n == 0 {
		return ErrSecretNotFound
	}
	return nil
}

// ResolveSecrets decrypts the listed names and returns name→plaintext.
// Names present at project scope win over globals; only names missing
// from the project fall back to the global table. Names not in either
// scope are silently omitted (caller decides whether missing is an
// error — scheduler treats it as pipeline misconfig, fails the job).
func (s *Store) ResolveSecrets(ctx context.Context, cipher *crypto.Cipher, projectID uuid.UUID, names []string) (map[string]string, error) {
	if cipher == nil {
		return nil, errors.New("store: secrets: cipher not configured")
	}
	if len(names) == 0 {
		return map[string]string{}, nil
	}
	rows, err := s.q.GetSecretValuesByProject(ctx, db.GetSecretValuesByProjectParams{
		ProjectID: pgUUID(projectID),
		Column2:   names,
	})
	if err != nil {
		return nil, fmt.Errorf("store: get secrets: %w", err)
	}
	out := make(map[string]string, len(names))
	for _, r := range rows {
		plain, err := cipher.Decrypt(r.ValueEnc)
		if err != nil {
			return nil, fmt.Errorf("store: decrypt secret %q: %w", r.Name, err)
		}
		out[r.Name] = string(plain)
	}

	// Fallback to globals for names the project didn't cover.
	// Short-circuit when everything resolved at project scope to
	// avoid the extra round trip on the hot path.
	missing := make([]string, 0, len(names)-len(out))
	for _, n := range names {
		if _, ok := out[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	globals, err := s.q.GetGlobalSecretValues(ctx, missing)
	if err != nil {
		return nil, fmt.Errorf("store: get global secrets: %w", err)
	}
	for _, r := range globals {
		plain, err := cipher.Decrypt(r.ValueEnc)
		if err != nil {
			return nil, fmt.Errorf("store: decrypt global secret %q: %w", r.Name, err)
		}
		out[r.Name] = string(plain)
	}
	return out, nil
}

// SetGlobalSecret encrypts plaintext and upserts at global scope
// (project_id = NULL). Reuses SecretSet's name/value pair with
// ProjectID ignored — admin-only path, gated on the HTTP side.
func (s *Store) SetGlobalSecret(ctx context.Context, cipher *crypto.Cipher, name string, value []byte) (bool, error) {
	if cipher == nil {
		return false, errors.New("store: secrets: cipher not configured")
	}
	if err := ValidateSecretName(name); err != nil {
		return false, err
	}
	enc, err := cipher.Encrypt(value)
	if err != nil {
		return false, fmt.Errorf("store: encrypt global secret: %w", err)
	}
	row, err := s.q.UpsertGlobalSecret(ctx, db.UpsertGlobalSecretParams{
		Name:     name,
		ValueEnc: enc,
	})
	if err != nil {
		return false, fmt.Errorf("store: upsert global secret: %w", err)
	}
	return row.Created, nil
}

// ListGlobalSecrets returns names + timestamps for every global
// row. Admin-only view — nothing here that a regular user should
// see (names leak infrastructure topology).
func (s *Store) ListGlobalSecrets(ctx context.Context) ([]Secret, error) {
	rows, err := s.q.ListGlobalSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list global secrets: %w", err)
	}
	out := make([]Secret, 0, len(rows))
	for _, r := range rows {
		out = append(out, Secret{
			Name:      r.Name,
			CreatedAt: r.CreatedAt.Time,
			UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// DeleteGlobalSecret removes a global row by name. Returns
// ErrSecretNotFound when nothing matched so the HTTP layer can
// 404 cleanly.
func (s *Store) DeleteGlobalSecret(ctx context.Context, name string) error {
	n, err := s.q.DeleteGlobalSecretByName(ctx, name)
	if err != nil {
		return fmt.Errorf("store: delete global secret: %w", err)
	}
	if n == 0 {
		return ErrSecretNotFound
	}
	return nil
}

// ValidateSecretName enforces the naming convention. Exposed so the HTTP
// handler can return a clean 400 before touching the DB.
func ValidateSecretName(name string) error {
	if !secretNamePattern.MatchString(name) {
		return fmt.Errorf("store: secret name must match %s (got %q)", secretNamePattern.String(), name)
	}
	return nil
}
