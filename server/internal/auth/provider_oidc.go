package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig covers every standard OIDC IdP. Google + Keycloak +
// corporate SSO all go through here — only the display name and
// issuer URL differ.
type OIDCConfig struct {
	Name        ProviderName
	DisplayText string
	Issuer      string
	ClientID    string
	ClientSecret string
	CallbackURL string
	// Scopes default to ["openid", "profile", "email"]. Providers
	// that require custom scopes (offline_access etc.) override it.
	Scopes []string
}

// NewOIDCProvider performs the discovery round-trip at boot so an
// unreachable IdP fails the startup, not the first login.
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig) (Provider, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.CallbackURL == "" {
		return nil, fmt.Errorf("oidc auth (%s): issuer + client id/secret + callback required", cfg.Name)
	}
	if cfg.Name == "" {
		cfg.Name = ProviderOIDC
	}
	prov, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc auth (%s): discovery: %w", cfg.Name, err)
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	oauthCfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     prov.Endpoint(),
		RedirectURL:  cfg.CallbackURL,
		Scopes:       scopes,
	}
	verifier := prov.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	return &oidcProvider{
		name:        cfg.Name,
		displayName: displayOrDefault(cfg.Name, cfg.DisplayText),
		cfg:         oauthCfg,
		verifier:    verifier,
	}, nil
}

type oidcProvider struct {
	name        ProviderName
	displayName string
	cfg         *oauth2.Config
	verifier    *oidc.IDTokenVerifier
}

func (p *oidcProvider) Name() ProviderName { return p.name }
func (p *oidcProvider) DisplayName() string { return p.displayName }

func (p *oidcProvider) AuthorizeURL(state, nonce string) string {
	extras := []oauth2.AuthCodeOption{oauth2.AccessTypeOnline}
	if nonce != "" {
		extras = append(extras, oidc.Nonce(nonce))
	}
	return p.cfg.AuthCodeURL(state, extras...)
}

func (p *oidcProvider) Exchange(ctx context.Context, code, _, nonce string) (Claims, error) {
	tok, err := p.cfg.Exchange(ctx, code)
	if err != nil {
		return Claims{}, fmt.Errorf("oidc auth (%s): exchange: %w", p.name, err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return Claims{}, fmt.Errorf("oidc auth (%s): missing id_token", p.name)
	}
	idTok, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return Claims{}, fmt.Errorf("oidc auth (%s): verify id_token: %w", p.name, err)
	}
	if nonce != "" && idTok.Nonce != nonce {
		return Claims{}, errors.New("oidc auth: nonce mismatch")
	}

	var raw struct {
		Subject   string `json:"sub"`
		Email     string `json:"email"`
		EmailVerified *bool `json:"email_verified,omitempty"`
		Name      string `json:"name"`
		Picture   string `json:"picture"`
		// Keycloak exposes `preferred_username` when the profile
		// has no name set; we fall back to it below.
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idTok.Claims(&raw); err != nil {
		return Claims{}, fmt.Errorf("oidc auth (%s): decode claims: %w", p.name, err)
	}
	if raw.Subject == "" || raw.Email == "" {
		return Claims{}, ErrClaimsMissing
	}
	if raw.EmailVerified != nil && !*raw.EmailVerified {
		return Claims{}, fmt.Errorf("oidc auth (%s): email not verified", p.name)
	}
	name := raw.Name
	if name == "" {
		name = raw.PreferredUsername
	}
	if name == "" {
		name = raw.Email
	}
	return Claims{
		Subject:   raw.Subject,
		Email:     raw.Email,
		Name:      name,
		AvatarURL: raw.Picture,
	}, nil
}

func displayOrDefault(name ProviderName, explicit string) string {
	if explicit != "" {
		return explicit
	}
	switch name {
	case ProviderGoogle:
		return "Google"
	case ProviderKeycloak:
		return "Keycloak"
	case ProviderOIDC:
		return "Single Sign-On"
	default:
		return string(name)
	}
}
