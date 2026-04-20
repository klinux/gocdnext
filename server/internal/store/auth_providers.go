package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/db"
)

// Provider-kind constants, mirrored from the migration's CHECK.
const (
	ProviderKindGitHub = "github"
	ProviderKindOIDC   = "oidc"
)

// Sentinel errors for the admin CRUD endpoints.
var (
	ErrAuthProviderNotFound   = errors.New("store: auth provider not found")
	ErrAuthProviderCipherUnset = errors.New("store: auth provider encryption not configured")
)

// ConfiguredProvider is the admin-facing shape. ClientSecret is
// never returned here — the plaintext lives only inside
// BootstrapProvider / UpsertConfiguredProvider flows.
type ConfiguredProvider struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	DisplayName   string    `json:"display_name"`
	ClientID      string    `json:"client_id"`
	Issuer        string    `json:"issuer,omitempty"`
	GitHubAPIBase string    `json:"github_api_base,omitempty"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// BootstrapProvider is the shape returned to the auth bootstrap
// path. ClientSecret is decrypted here — never expose this struct
// to HTTP callers.
type BootstrapProvider struct {
	ConfiguredProvider
	ClientSecret string
}

// UpsertAuthProviderInput is the admin write payload. Pass
// ClientSecret="" on update to keep the existing ciphertext (the
// form in the UI renders the secret as "••••" and only sends a
// fresh value when the admin types one).
type UpsertAuthProviderInput struct {
	Name          string
	Kind          string
	DisplayName   string
	ClientID      string
	ClientSecret  string // plaintext; empty = preserve existing
	Issuer        string
	GitHubAPIBase string
	Enabled       bool
}

// SetCipher wires the AES-GCM cipher used to seal client_secret at
// rest. Without it every auth-provider write + bootstrap read
// returns ErrAuthProviderCipherUnset. Intentionally distinct from
// the secrets cipher wiring so an operator can use separate keys
// for jobs secrets vs auth (they tend to rotate on different
// cadences); practically we pass the same Cipher in main.go.
func (s *Store) SetAuthCipher(c *crypto.Cipher) {
	s.authCipher = c
}

// ListConfiguredProviders is the admin-page feed. Secrets never
// leave the store through this path.
func (s *Store) ListConfiguredProviders(ctx context.Context) ([]ConfiguredProvider, error) {
	rows, err := s.q.ListAuthProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list auth providers: %w", err)
	}
	out := make([]ConfiguredProvider, 0, len(rows))
	for _, r := range rows {
		out = append(out, ConfiguredProvider{
			ID:            fromPgUUID(r.ID),
			Name:          r.Name,
			Kind:          r.Kind,
			DisplayName:   r.DisplayName,
			ClientID:      r.ClientID,
			Issuer:        r.Issuer,
			GitHubAPIBase: r.GithubApiBase,
			Enabled:       r.Enabled,
			CreatedAt:     r.CreatedAt.Time,
			UpdatedAt:     r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// ListBootstrapProviders decrypts client_secret for each enabled
// row. Intended for the boot + reload paths only.
func (s *Store) ListBootstrapProviders(ctx context.Context) ([]BootstrapProvider, error) {
	if s.authCipher == nil {
		return nil, ErrAuthProviderCipherUnset
	}
	rows, err := s.q.ListEnabledAuthProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled auth providers: %w", err)
	}
	out := make([]BootstrapProvider, 0, len(rows))
	for _, r := range rows {
		plain, err := s.authCipher.Decrypt(r.ClientSecret)
		if err != nil {
			return nil, fmt.Errorf("store: decrypt auth provider %s: %w", r.Name, err)
		}
		out = append(out, BootstrapProvider{
			ConfiguredProvider: ConfiguredProvider{
				ID:            fromPgUUID(r.ID),
				Name:          r.Name,
				Kind:          r.Kind,
				DisplayName:   r.DisplayName,
				ClientID:      r.ClientID,
				Issuer:        r.Issuer,
				GitHubAPIBase: r.GithubApiBase,
				Enabled:       r.Enabled,
				CreatedAt:     r.CreatedAt.Time,
				UpdatedAt:     r.UpdatedAt.Time,
			},
			ClientSecret: string(plain),
		})
	}
	return out, nil
}

// UpsertConfiguredProvider creates or updates by name. Empty
// ClientSecret on update preserves the existing ciphertext.
func (s *Store) UpsertConfiguredProvider(ctx context.Context, in UpsertAuthProviderInput) (ConfiguredProvider, error) {
	if s.authCipher == nil {
		return ConfiguredProvider{}, ErrAuthProviderCipherUnset
	}
	if in.Name == "" || in.ClientID == "" {
		return ConfiguredProvider{}, errors.New("store: provider name + client id required")
	}
	if in.Kind != ProviderKindGitHub && in.Kind != ProviderKindOIDC {
		return ConfiguredProvider{}, fmt.Errorf("store: invalid kind %q", in.Kind)
	}

	var ciphertext []byte
	if in.ClientSecret == "" {
		// Preserve existing secret. Look up by name; require the row
		// to exist (empty secret on first insert is a validation
		// error the UI surfaces client-side).
		existing, err := s.findByName(ctx, in.Name)
		if err != nil {
			return ConfiguredProvider{}, err
		}
		ciphertext = existing.ClientSecret
	} else {
		sealed, err := s.authCipher.Encrypt([]byte(in.ClientSecret))
		if err != nil {
			return ConfiguredProvider{}, fmt.Errorf("store: encrypt client secret: %w", err)
		}
		ciphertext = sealed
	}

	row, err := s.q.UpsertAuthProvider(ctx, db.UpsertAuthProviderParams{
		Name:          in.Name,
		Kind:          in.Kind,
		DisplayName:   in.DisplayName,
		ClientID:      in.ClientID,
		ClientSecret:  ciphertext,
		Issuer:        in.Issuer,
		GithubApiBase: in.GitHubAPIBase,
		Enabled:       in.Enabled,
	})
	if err != nil {
		return ConfiguredProvider{}, fmt.Errorf("store: upsert auth provider: %w", err)
	}
	return ConfiguredProvider{
		ID:            fromPgUUID(row.ID),
		Name:          row.Name,
		Kind:          row.Kind,
		DisplayName:   row.DisplayName,
		ClientID:      row.ClientID,
		Issuer:        row.Issuer,
		GitHubAPIBase: row.GithubApiBase,
		Enabled:       row.Enabled,
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     row.UpdatedAt.Time,
	}, nil
}

// DeleteConfiguredProvider removes a row by id. Missing rows are
// surfaced as ErrAuthProviderNotFound so the handler can 404.
func (s *Store) DeleteConfiguredProvider(ctx context.Context, id uuid.UUID) error {
	// Two-step so we can distinguish "not found" from "delete
	// failed"; the happy path is one query.
	if _, err := s.q.GetAuthProviderByID(ctx, pgUUID(id)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAuthProviderNotFound
		}
		return fmt.Errorf("store: find auth provider: %w", err)
	}
	if err := s.q.DeleteAuthProvider(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("store: delete auth provider: %w", err)
	}
	return nil
}

// SetAuthProviderEnabled flips the enabled boolean without touching
// the rest of the row.
func (s *Store) SetAuthProviderEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	return s.q.SetAuthProviderEnabled(ctx, db.SetAuthProviderEnabledParams{
		ID:      pgUUID(id),
		Enabled: enabled,
	})
}

func (s *Store) findByName(ctx context.Context, name string) (db.AuthProvider, error) {
	rows, err := s.q.ListAuthProviders(ctx)
	if err != nil {
		return db.AuthProvider{}, fmt.Errorf("store: list auth providers: %w", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r, nil
		}
	}
	return db.AuthProvider{}, ErrAuthProviderNotFound
}
