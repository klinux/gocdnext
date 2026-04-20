package auth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/config"
)

// BuildRegistry turns the env-derived Config into a Provider Registry.
// Providers with empty client id are silently skipped (that's the
// "disabled" signal). Each configured provider needs PublicBase to
// be set so the callback URL can be computed.
//
// Callers should treat a boot-time error as fatal: better to fail
// startup than to run half-configured auth.
func BuildRegistry(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Registry, error) {
	if !cfg.AuthEnabled {
		return NewRegistry(), nil
	}
	if cfg.PublicBase == "" {
		return nil, fmt.Errorf("auth: GOCDNEXT_PUBLIC_BASE required when auth is enabled")
	}
	callback := func(name string) string {
		base := strings.TrimRight(cfg.PublicBase, "/")
		return fmt.Sprintf("%s/auth/callback/%s", base, name)
	}

	var providers []Provider

	if cfg.AuthGitHubClientID != "" {
		p, err := NewGitHubProvider(GitHubConfig{
			ClientID:     cfg.AuthGitHubClientID,
			ClientSecret: cfg.AuthGitHubClientSecret,
			CallbackURL:  callback(string(ProviderGitHub)),
			APIBase:      cfg.AuthGitHubAPIBase,
		})
		if err != nil {
			return nil, fmt.Errorf("auth: github provider: %w", err)
		}
		providers = append(providers, p)
		log.Info("auth: provider enabled", "name", ProviderGitHub)
	}

	if cfg.AuthGoogleClientID != "" {
		p, err := NewOIDCProvider(ctx, OIDCConfig{
			Name:         ProviderGoogle,
			Issuer:       cfg.AuthGoogleIssuer,
			ClientID:     cfg.AuthGoogleClientID,
			ClientSecret: cfg.AuthGoogleClientSecret,
			CallbackURL:  callback(string(ProviderGoogle)),
		})
		if err != nil {
			return nil, fmt.Errorf("auth: google provider: %w", err)
		}
		providers = append(providers, p)
		log.Info("auth: provider enabled", "name", ProviderGoogle)
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
			CallbackURL:  callback(string(ProviderKeycloak)),
		})
		if err != nil {
			return nil, fmt.Errorf("auth: keycloak provider: %w", err)
		}
		providers = append(providers, p)
		log.Info("auth: provider enabled", "name", ProviderKeycloak)
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
			CallbackURL:  callback(string(ProviderOIDC)),
			DisplayText:  cfg.AuthOIDCDisplayName,
		})
		if err != nil {
			return nil, fmt.Errorf("auth: oidc provider: %w", err)
		}
		providers = append(providers, p)
		log.Info("auth: provider enabled", "name", ProviderOIDC)
	}

	if len(providers) == 0 {
		log.Warn("auth: enabled but no providers configured — login page will be empty")
	}
	return NewRegistry(providers...), nil
}
