package auth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/config"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// DBProviderSource is the minimal contract BuildRegistry needs to
// pull persisted provider rows. In prod it's the full *store.Store;
// tests can pass a fake.
type DBProviderSource interface {
	ListBootstrapProviders(ctx context.Context) ([]store.BootstrapProvider, error)
}

// BuildRegistry merges env-based providers with DB-stored providers
// and returns a Registry ready to serve. DB rows override env rows
// sharing the same name, so an operator who configures GitHub via
// env can later rotate credentials by writing the DB row without
// touching the env.
//
// A nil dbSource is valid (auth disabled, or DB not reachable at
// bootstrap); only env providers are loaded in that case.
func BuildRegistry(ctx context.Context, cfg *config.Config, dbSource DBProviderSource, log *slog.Logger) (*Registry, error) {
	if !cfg.AuthEnabled {
		return NewRegistry(), nil
	}
	if cfg.PublicBase == "" {
		return nil, fmt.Errorf("auth: GOCDNEXT_PUBLIC_BASE required when auth is enabled")
	}

	envProviders, err := buildEnvProviders(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	dbProviders, err := buildDBProviders(ctx, cfg, dbSource, log)
	if err != nil {
		// DB failures on boot shouldn't permanently stall the
		// server — warn loudly and fall back to env-only. The
		// admin UI can re-trigger a reload once the DB is back.
		log.Warn("auth: DB provider load failed, continuing with env-only", "err", err)
	}

	combined := append(envProviders, dbProviders...)
	if len(combined) == 0 {
		log.Warn("auth: enabled but no providers configured — login page will be empty")
	}
	return NewRegistry(combined...), nil
}

// Reload rebuilds the Registry in place. Call this after a CRUD
// write so the new config takes effect without a restart.
func Reload(ctx context.Context, registry *Registry, cfg *config.Config, dbSource DBProviderSource, log *slog.Logger) error {
	if !cfg.AuthEnabled {
		registry.Replace()
		return nil
	}
	envProviders, err := buildEnvProviders(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("auth reload: env: %w", err)
	}
	dbProviders, err := buildDBProviders(ctx, cfg, dbSource, log)
	if err != nil {
		return fmt.Errorf("auth reload: db: %w", err)
	}
	registry.Replace(append(envProviders, dbProviders...)...)
	log.Info("auth: registry reloaded",
		"env", len(envProviders), "db", len(dbProviders))
	return nil
}

func callbackURL(cfg *config.Config, name string) string {
	base := strings.TrimRight(cfg.PublicBase, "/")
	return fmt.Sprintf("%s/auth/callback/%s", base, name)
}

func buildEnvProviders(ctx context.Context, cfg *config.Config, log *slog.Logger) ([]Provider, error) {
	var out []Provider

	if cfg.AuthGitHubClientID != "" {
		p, err := NewGitHubProvider(GitHubConfig{
			ClientID:     cfg.AuthGitHubClientID,
			ClientSecret: cfg.AuthGitHubClientSecret,
			CallbackURL:  callbackURL(cfg, string(ProviderGitHub)),
			APIBase:      cfg.AuthGitHubAPIBase,
		})
		if err != nil {
			return nil, fmt.Errorf("auth: github provider: %w", err)
		}
		out = append(out, p)
		log.Info("auth: provider from env", "name", ProviderGitHub)
	}

	if cfg.AuthGoogleClientID != "" {
		p, err := NewOIDCProvider(ctx, OIDCConfig{
			Name:         ProviderGoogle,
			Issuer:       cfg.AuthGoogleIssuer,
			ClientID:     cfg.AuthGoogleClientID,
			ClientSecret: cfg.AuthGoogleClientSecret,
			CallbackURL:  callbackURL(cfg, string(ProviderGoogle)),
		})
		if err != nil {
			return nil, fmt.Errorf("auth: google provider: %w", err)
		}
		out = append(out, p)
		log.Info("auth: provider from env", "name", ProviderGoogle)
	}

	if cfg.AuthKeycloakClientID != "" {
		if cfg.AuthKeycloakIssuer == "" {
			return nil, fmt.Errorf("auth: GOCDNEXT_AUTH_KEYCLOAK_ISSUER required when KEYCLOAK_CLIENT_ID is set")
		}
		p, err := NewOIDCProvider(ctx, OIDCConfig{
			Name:         ProviderKeycloak,
			Issuer:       cfg.AuthKeycloakIssuer,
			ClientID:     cfg.AuthKeycloakClientID,
			ClientSecret: cfg.AuthKeycloakClientSecret,
			CallbackURL:  callbackURL(cfg, string(ProviderKeycloak)),
		})
		if err != nil {
			return nil, fmt.Errorf("auth: keycloak provider: %w", err)
		}
		out = append(out, p)
		log.Info("auth: provider from env", "name", ProviderKeycloak)
	}

	if cfg.AuthOIDCClientID != "" {
		if cfg.AuthOIDCIssuer == "" {
			return nil, fmt.Errorf("auth: GOCDNEXT_AUTH_OIDC_ISSUER required when OIDC_CLIENT_ID is set")
		}
		p, err := NewOIDCProvider(ctx, OIDCConfig{
			Name:         ProviderOIDC,
			Issuer:       cfg.AuthOIDCIssuer,
			ClientID:     cfg.AuthOIDCClientID,
			ClientSecret: cfg.AuthOIDCClientSecret,
			CallbackURL:  callbackURL(cfg, string(ProviderOIDC)),
			DisplayText:  cfg.AuthOIDCDisplayName,
		})
		if err != nil {
			return nil, fmt.Errorf("auth: oidc provider: %w", err)
		}
		out = append(out, p)
		log.Info("auth: provider from env", "name", ProviderOIDC)
	}

	return out, nil
}

// buildDBProviders decrypts stored config rows and instantiates a
// Provider for each. Individual provider failures log + skip so one
// misconfigured row doesn't take down the whole login page.
func buildDBProviders(ctx context.Context, cfg *config.Config, src DBProviderSource, log *slog.Logger) ([]Provider, error) {
	if src == nil {
		return nil, nil
	}
	rows, err := src.ListBootstrapProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Provider, 0, len(rows))
	for _, row := range rows {
		p, err := instantiateFromRow(ctx, cfg, row)
		if err != nil {
			log.Warn("auth: db provider skipped",
				"name", row.Name, "kind", row.Kind, "err", err)
			continue
		}
		out = append(out, p)
		log.Info("auth: provider from db", "name", row.Name, "kind", row.Kind)
	}
	return out, nil
}

func instantiateFromRow(ctx context.Context, cfg *config.Config, row store.BootstrapProvider) (Provider, error) {
	callback := callbackURL(cfg, row.Name)
	switch row.Kind {
	case store.ProviderKindGitHub:
		return NewGitHubProvider(GitHubConfig{
			ClientID:     row.ClientID,
			ClientSecret: row.ClientSecret,
			CallbackURL:  callback,
			APIBase:      row.GitHubAPIBase,
		})
	case store.ProviderKindOIDC:
		return NewOIDCProvider(ctx, OIDCConfig{
			Name:         ProviderName(row.Name),
			Issuer:       row.Issuer,
			ClientID:     row.ClientID,
			ClientSecret: row.ClientSecret,
			CallbackURL:  callback,
			DisplayText:  row.DisplayName,
		})
	default:
		return nil, fmt.Errorf("unsupported kind %q", row.Kind)
	}
}
