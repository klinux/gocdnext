package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// VCS integration kinds, mirrored from the migration's CHECK.
const (
	VCSKindGitHubApp = "github_app"
)

// Sentinel errors for the admin CRUD endpoints. Cipher-unset reuses
// the auth provider sentinel — same underlying GOCDNEXT_SECRET_KEY
// pipe, so "encryption not configured" is a single concept.
var (
	ErrVCSIntegrationNotFound = errors.New("store: vcs integration not found")
)

// ConfiguredVCSIntegration is the admin-facing shape. Secret
// material (private_key, webhook_secret) never leaves the store
// through this struct — only BootstrapVCSIntegration exposes the
// decrypted values, and only to the registry path.
type ConfiguredVCSIntegration struct {
	ID          uuid.UUID `json:"id"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	AppID       *int64    `json:"app_id,omitempty"`
	APIBase     string    `json:"api_base,omitempty"`
	Enabled     bool      `json:"enabled"`
	// HasPrivateKey / HasWebhookSecret tell the UI whether the
	// row already carries a secret so the form can render "••••"
	// instead of looking empty.
	HasPrivateKey    bool      `json:"has_private_key"`
	HasWebhookSecret bool      `json:"has_webhook_secret"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// BootstrapVCSIntegration carries the decrypted secrets. Never
// expose this struct to HTTP callers — the registry + checks
// reporter are the only consumers.
type BootstrapVCSIntegration struct {
	ConfiguredVCSIntegration
	PrivateKeyPEM []byte
	WebhookSecret string
}

// UpsertVCSIntegrationInput is the admin write payload. Empty
// PrivateKeyPEM / WebhookSecret on update preserves the existing
// ciphertext (the UI renders stored secrets as "••••" and only
// sends fresh values when the admin types new ones).
type UpsertVCSIntegrationInput struct {
	Name           string
	Kind           string
	DisplayName    string
	AppID          *int64
	PrivateKeyPEM  []byte // plaintext; empty = preserve existing
	WebhookSecret  string // plaintext; empty = preserve existing
	APIBase        string
	Enabled        bool
}

// ListConfiguredVCSIntegrations is the admin-page feed.
func (s *Store) ListConfiguredVCSIntegrations(ctx context.Context) ([]ConfiguredVCSIntegration, error) {
	rows, err := s.q.ListVCSIntegrations(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list vcs integrations: %w", err)
	}
	out := make([]ConfiguredVCSIntegration, 0, len(rows))
	for _, r := range rows {
		out = append(out, ConfiguredVCSIntegration{
			ID:               fromPgUUID(r.ID),
			Kind:             r.Kind,
			Name:             r.Name,
			DisplayName:      r.DisplayName,
			AppID:            int64Ptr(r.AppID),
			APIBase:          r.ApiBase,
			Enabled:          r.Enabled,
			HasPrivateKey:    len(r.PrivateKey) > 0,
			HasWebhookSecret: len(r.WebhookSecret) > 0,
			CreatedAt:        r.CreatedAt.Time,
			UpdatedAt:        r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// ListBootstrapVCSIntegrations decrypts secrets for each enabled
// row. Intended for the registry boot + reload paths only.
func (s *Store) ListBootstrapVCSIntegrations(ctx context.Context) ([]BootstrapVCSIntegration, error) {
	if s.authCipher == nil {
		return nil, ErrAuthProviderCipherUnset
	}
	rows, err := s.q.ListEnabledVCSIntegrations(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled vcs integrations: %w", err)
	}
	out := make([]BootstrapVCSIntegration, 0, len(rows))
	for _, r := range rows {
		cfg := ConfiguredVCSIntegration{
			ID:               fromPgUUID(r.ID),
			Kind:             r.Kind,
			Name:             r.Name,
			DisplayName:      r.DisplayName,
			AppID:            int64Ptr(r.AppID),
			APIBase:          r.ApiBase,
			Enabled:          r.Enabled,
			HasPrivateKey:    len(r.PrivateKey) > 0,
			HasWebhookSecret: len(r.WebhookSecret) > 0,
			CreatedAt:        r.CreatedAt.Time,
			UpdatedAt:        r.UpdatedAt.Time,
		}
		var pem []byte
		if len(r.PrivateKey) > 0 {
			plain, err := s.authCipher.Decrypt(r.PrivateKey)
			if err != nil {
				return nil, fmt.Errorf("store: decrypt private key %s: %w", r.Name, err)
			}
			pem = plain
		}
		var secret string
		if len(r.WebhookSecret) > 0 {
			plain, err := s.authCipher.Decrypt(r.WebhookSecret)
			if err != nil {
				return nil, fmt.Errorf("store: decrypt webhook secret %s: %w", r.Name, err)
			}
			secret = string(plain)
		}
		out = append(out, BootstrapVCSIntegration{
			ConfiguredVCSIntegration: cfg,
			PrivateKeyPEM:            pem,
			WebhookSecret:            secret,
		})
	}
	return out, nil
}

// UpsertVCSIntegration creates or updates by name. Empty
// PrivateKeyPEM / WebhookSecret on update preserve the existing
// ciphertext.
func (s *Store) UpsertVCSIntegration(ctx context.Context, in UpsertVCSIntegrationInput) (ConfiguredVCSIntegration, error) {
	if s.authCipher == nil {
		return ConfiguredVCSIntegration{}, ErrAuthProviderCipherUnset
	}
	if in.Name == "" {
		return ConfiguredVCSIntegration{}, errors.New("store: vcs integration name required")
	}
	if in.Kind != VCSKindGitHubApp {
		return ConfiguredVCSIntegration{}, fmt.Errorf("store: unsupported vcs kind %q", in.Kind)
	}
	// GitHub App requires app_id + private key on first insert.
	// Subsequent updates may omit private_key (preserved via
	// COALESCE in the query) but app_id must always be present.
	if in.Kind == VCSKindGitHubApp && (in.AppID == nil || *in.AppID <= 0) {
		return ConfiguredVCSIntegration{}, errors.New("store: github_app requires a positive app_id")
	}

	var pkCipher []byte
	if len(in.PrivateKeyPEM) > 0 {
		sealed, err := s.authCipher.Encrypt(in.PrivateKeyPEM)
		if err != nil {
			return ConfiguredVCSIntegration{}, fmt.Errorf("store: encrypt private key: %w", err)
		}
		pkCipher = sealed
	}
	var whCipher []byte
	if in.WebhookSecret != "" {
		sealed, err := s.authCipher.Encrypt([]byte(in.WebhookSecret))
		if err != nil {
			return ConfiguredVCSIntegration{}, fmt.Errorf("store: encrypt webhook secret: %w", err)
		}
		whCipher = sealed
	}

	row, err := s.q.UpsertVCSIntegration(ctx, db.UpsertVCSIntegrationParams{
		Kind:          in.Kind,
		Name:          in.Name,
		DisplayName:   in.DisplayName,
		AppID:         appIDParam(in.AppID),
		PrivateKey:    pkCipher,
		WebhookSecret: whCipher,
		ApiBase:       in.APIBase,
		Enabled:       in.Enabled,
	})
	if err != nil {
		return ConfiguredVCSIntegration{}, fmt.Errorf("store: upsert vcs integration: %w", err)
	}
	return ConfiguredVCSIntegration{
		ID:               fromPgUUID(row.ID),
		Kind:             row.Kind,
		Name:             row.Name,
		DisplayName:      row.DisplayName,
		AppID:            int64Ptr(row.AppID),
		APIBase:          row.ApiBase,
		Enabled:          row.Enabled,
		HasPrivateKey:    len(row.PrivateKey) > 0,
		HasWebhookSecret: len(row.WebhookSecret) > 0,
		CreatedAt:        row.CreatedAt.Time,
		UpdatedAt:        row.UpdatedAt.Time,
	}, nil
}

// DeleteVCSIntegration removes a row by id. Missing id →
// ErrVCSIntegrationNotFound.
func (s *Store) DeleteVCSIntegration(ctx context.Context, id uuid.UUID) error {
	if _, err := s.q.GetVCSIntegrationByID(ctx, pgUUID(id)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrVCSIntegrationNotFound
		}
		return fmt.Errorf("store: find vcs integration: %w", err)
	}
	if err := s.q.DeleteVCSIntegration(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("store: delete vcs integration: %w", err)
	}
	return nil
}

// SetVCSIntegrationEnabled flips the boolean without touching the
// rest of the row.
func (s *Store) SetVCSIntegrationEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	return s.q.SetVCSIntegrationEnabled(ctx, db.SetVCSIntegrationEnabledParams{
		ID:      pgUUID(id),
		Enabled: enabled,
	})
}

// int64Ptr converts the nullable *int64 that pgx returns for
// BIGINT NULL into our JSON-friendly pointer.
func int64Ptr(v *int64) *int64 {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}

// appIDParam packs the *int64 we take from the handler back into
// the *int64 the generated sqlc params expect.
func appIDParam(v *int64) *int64 {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}
