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

// Secret sources. A `db` secret carries an encrypted value (today's model);
// the external sources carry a {ref_path, ref_key} pointer and NO value —
// the value lives in Vault / GCP Secret Manager / AWS Secrets Manager and is
// fetched at dispatch by the composite resolver.
const (
	SecretSourceDB    = "db"
	SecretSourceVault = "vault"
	SecretSourceGCP   = "gcp"
	SecretSourceAWS   = "aws"
)

// externalSources is the set of non-db sources the store accepts. Whether a
// referenced backend is actually CONFIGURED is enforced higher up (API +
// resolver); the store only validates the entry's shape.
var externalSources = map[string]bool{
	SecretSourceVault: true,
	SecretSourceGCP:   true,
	SecretSourceAWS:   true,
}

// SecretName pattern: letters, digits, underscore, up to 64 chars, must start
// with a letter. Keeps secret names safe to turn straight into env var names.
var secretNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// SecretRef is the external pointer (never a value).
type SecretRef struct {
	Path string `json:"path"`
	Key  string `json:"key,omitempty"`
}

// Secret is the list-side view: no value, ever. `source` + optional `ref`
// let ops audit "which secrets, from where" without reading any value.
type Secret struct {
	Name      string     `json:"name"`
	Source    string     `json:"source"`
	Ref       *SecretRef `json:"ref,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// SecretSet is the write-side input. For db, Value carries plaintext (held in
// memory only long enough for Encrypt). For an external source, Value is empty
// and RefPath (+ optional RefKey) point into the backend.
type SecretSet struct {
	ProjectID uuid.UUID
	Name      string
	Source    string // "" → db
	Value     []byte
	RefPath   string
	RefKey    string
}

// SecretEntry is the raw dispatch-time row. db rows carry ValueEnc; external
// rows carry Source + Ref. The CompositeResolver decrypts or fetches per
// source — the store does NOT decrypt here.
type SecretEntry struct {
	Name     string
	Source   string
	ValueEnc []byte
	RefPath  string
	RefKey   string
}

func normalizeSource(s string) string {
	if s == "" {
		return SecretSourceDB
	}
	return s
}

// validateSecretShape enforces the db-vs-external invariant before the upsert
// (defence in depth alongside the secrets_source_shape CHECK constraint).
func validateSecretShape(in SecretSet) error {
	switch src := normalizeSource(in.Source); {
	case src == SecretSourceDB:
		if in.RefPath != "" || in.RefKey != "" {
			return errors.New("store: db secret takes no ref_path/ref_key")
		}
	case externalSources[src]:
		if len(in.Value) > 0 {
			return fmt.Errorf("store: %s secret takes no value (it's a reference)", src)
		}
		if in.RefPath == "" {
			return fmt.Errorf("store: %s secret needs a ref_path", src)
		}
	default:
		return fmt.Errorf("store: unknown secret source %q", src)
	}
	return nil
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// derefStr lives in poll_state.go (same package) — reused here.

// SetSecret upserts a project secret. db source encrypts the value; an
// external source stores the pointer with no value. Returns created.
func (s *Store) SetSecret(ctx context.Context, cipher *crypto.Cipher, in SecretSet) (bool, error) {
	if err := ValidateSecretName(in.Name); err != nil {
		return false, err
	}
	if err := validateSecretShape(in); err != nil {
		return false, err
	}
	src := normalizeSource(in.Source)
	var enc []byte
	if src == SecretSourceDB {
		if cipher == nil {
			return false, errors.New("store: secrets: cipher not configured")
		}
		var err error
		if enc, err = cipher.Encrypt(in.Value); err != nil {
			return false, fmt.Errorf("store: encrypt secret: %w", err)
		}
	}
	row, err := s.q.UpsertSecret(ctx, db.UpsertSecretParams{
		ProjectID: pgUUID(in.ProjectID),
		Name:      in.Name,
		ValueEnc:  enc,
		Source:    src,
		RefPath:   strPtrOrNil(in.RefPath),
		RefKey:    strPtrOrNil(in.RefKey),
	})
	if err != nil {
		return false, fmt.Errorf("store: upsert secret: %w", err)
	}
	return row.Created, nil
}

// SetGlobalSecret upserts at global scope (project_id = NULL). ProjectID on
// the input is ignored — admin-only path, gated on the HTTP side.
func (s *Store) SetGlobalSecret(ctx context.Context, cipher *crypto.Cipher, in SecretSet) (bool, error) {
	if err := ValidateSecretName(in.Name); err != nil {
		return false, err
	}
	if err := validateSecretShape(in); err != nil {
		return false, err
	}
	src := normalizeSource(in.Source)
	var enc []byte
	if src == SecretSourceDB {
		if cipher == nil {
			return false, errors.New("store: secrets: cipher not configured")
		}
		var err error
		if enc, err = cipher.Encrypt(in.Value); err != nil {
			return false, fmt.Errorf("store: encrypt global secret: %w", err)
		}
	}
	row, err := s.q.UpsertGlobalSecret(ctx, db.UpsertGlobalSecretParams{
		Name:     in.Name,
		ValueEnc: enc,
		Source:   src,
		RefPath:  strPtrOrNil(in.RefPath),
		RefKey:   strPtrOrNil(in.RefKey),
	})
	if err != nil {
		return false, fmt.Errorf("store: upsert global secret: %w", err)
	}
	return row.Created, nil
}

// DeleteSecret removes a secret by name (404 via ErrSecretNotFound).
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

// DeleteGlobalSecret removes a global row by name (404 via ErrSecretNotFound).
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

// ResolveSecretEntries returns the raw entries for the named secrets, project
// scope shadowing global. Names absent in both scopes are silently omitted
// (the scheduler's diff reports them). No decryption / no external fetch here
// — the resolver dispatches per entry source.
func (s *Store) ResolveSecretEntries(ctx context.Context, projectID uuid.UUID, names []string) ([]SecretEntry, error) {
	if len(names) == 0 {
		return nil, nil
	}
	prows, err := s.q.GetSecretEntriesByProject(ctx, db.GetSecretEntriesByProjectParams{
		ProjectID: pgUUID(projectID),
		Column2:   names,
	})
	if err != nil {
		return nil, fmt.Errorf("store: get secret entries: %w", err)
	}
	out := make([]SecretEntry, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, r := range prows {
		out = append(out, SecretEntry{Name: r.Name, Source: r.Source, ValueEnc: r.ValueEnc, RefPath: derefStr(r.RefPath), RefKey: derefStr(r.RefKey)})
		seen[r.Name] = true
	}
	missing := make([]string, 0, len(names)-len(seen))
	for _, n := range names {
		if !seen[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	grows, err := s.q.GetGlobalSecretEntries(ctx, missing)
	if err != nil {
		return nil, fmt.Errorf("store: get global secret entries: %w", err)
	}
	for _, r := range grows {
		out = append(out, SecretEntry{Name: r.Name, Source: r.Source, ValueEnc: r.ValueEnc, RefPath: derefStr(r.RefPath), RefKey: derefStr(r.RefKey)})
	}
	return out, nil
}

// ResolveSecrets is the pure-DB resolver path (DBResolver). It decrypts
// db-source entries. An external-source entry on this path means config drift
// — a reference was created while a backend was configured, and that backend
// is no longer enabled. Failing loud (citing the NAME) surfaces the drift,
// instead of silently omitting it and letting the scheduler report the
// misleading "secrets not set on project". Project shadows global via
// ResolveSecretEntries.
func (s *Store) ResolveSecrets(ctx context.Context, cipher *crypto.Cipher, projectID uuid.UUID, names []string) (map[string]string, error) {
	entries, err := s.ResolveSecretEntries(ctx, projectID, names)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Source != SecretSourceDB {
			return nil, fmt.Errorf("store: secret %q references backend %q which is not configured on this server", e.Name, e.Source)
		}
		if cipher == nil {
			return nil, errors.New("store: secrets: cipher not configured")
		}
		plain, derr := cipher.Decrypt(e.ValueEnc)
		if derr != nil {
			return nil, fmt.Errorf("store: decrypt secret %q: %w", e.Name, derr)
		}
		out[e.Name] = string(plain)
	}
	return out, nil
}

// ListSecrets returns names + source/ref + timestamps (no value). Unpaginated;
// used for the inherited-globals panel. See secrets_paged.go for the paged list.
func (s *Store) ListSecrets(ctx context.Context, projectID uuid.UUID) ([]Secret, error) {
	rows, err := s.q.ListSecretsByProject(ctx, pgUUID(projectID))
	if err != nil {
		return nil, fmt.Errorf("store: list secrets: %w", err)
	}
	out := make([]Secret, 0, len(rows))
	for _, r := range rows {
		out = append(out, secretView(r.Name, r.Source, r.RefPath, r.RefKey, r.CreatedAt.Time, r.UpdatedAt.Time))
	}
	return out, nil
}

// ListGlobalSecrets returns every global row's name + source/ref (no value).
func (s *Store) ListGlobalSecrets(ctx context.Context) ([]Secret, error) {
	rows, err := s.q.ListGlobalSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list global secrets: %w", err)
	}
	out := make([]Secret, 0, len(rows))
	for _, r := range rows {
		out = append(out, secretView(r.Name, r.Source, r.RefPath, r.RefKey, r.CreatedAt.Time, r.UpdatedAt.Time))
	}
	return out, nil
}

// secretView builds the value-free list view, attaching a ref only for
// external sources.
func secretView(name, source string, refPath, refKey *string, created, updated time.Time) Secret {
	sec := Secret{Name: name, Source: source, CreatedAt: created, UpdatedAt: updated}
	if source != SecretSourceDB {
		sec.Ref = &SecretRef{Path: derefStr(refPath), Key: derefStr(refKey)}
	}
	return sec
}

// ValidateSecretName enforces the naming convention. Exposed so the HTTP
// handler can return a clean 400 before touching the DB.
func ValidateSecretName(name string) error {
	if !secretNamePattern.MatchString(name) {
		return fmt.Errorf("store: secret name must match %s (got %q)", secretNamePattern.String(), name)
	}
	return nil
}

// ValidateSecretRef is the API-layer check for a write's source + reference,
// given the set of backends configured on THIS server. db takes no ref; an
// external source must be configured and carry a ref_path (Vault also needs a
// ref_key, since a Vault secret is a key/value map). The store's own
// validateSecretShape is the DB-edge guard; this one adds the
// configured-on-this-server + vault-key rules the handler needs for a clean
// 400.
func ValidateSecretRef(source, refPath, refKey string, configured map[string]bool) error {
	src := normalizeSource(source)
	if src == SecretSourceDB {
		if refPath != "" || refKey != "" {
			return errors.New("db secret takes no ref path/key")
		}
		return nil
	}
	if !externalSources[src] {
		return fmt.Errorf("unknown secret source %q", src)
	}
	if !configured[src] {
		return fmt.Errorf("secret source %q is not configured on this server", src)
	}
	if refPath == "" {
		return fmt.Errorf("%s secret needs a ref path", src)
	}
	if src == SecretSourceVault && refKey == "" {
		return errors.New("vault secret needs a ref key (a Vault secret is a key/value map)")
	}
	return nil
}
