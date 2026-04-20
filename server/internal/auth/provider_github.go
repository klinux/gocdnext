package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
)

// GitHubConfig captures the env-var inputs for the GitHub provider.
// CallbackURL is the absolute URL the IdP calls back — we build it
// from GOCDNEXT_PUBLIC_BASE at boot and pass it in here.
type GitHubConfig struct {
	ClientID     string
	ClientSecret string
	CallbackURL  string
	// APIBase overrides api.github.com for GitHub Enterprise
	// installations. Empty = github.com.
	APIBase string
	// Scopes default to `read:user user:email` when empty. Override
	// only if you need extra repos access (we don't here).
	Scopes []string
	// HTTPClient lets tests inject a round-tripper that serves
	// /user and /user/emails from a stub.
	HTTPClient *http.Client
}

// NewGitHubProvider builds a GitHub provider. Returns an error when
// required fields are missing so main.go can surface the misconfig
// at startup instead of at first login.
func NewGitHubProvider(cfg GitHubConfig) (Provider, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("github auth: client id + secret required")
	}
	if cfg.CallbackURL == "" {
		return nil, fmt.Errorf("github auth: callback URL required")
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"read:user", "user:email"}
	}
	ep := github.Endpoint
	apiBase := "https://api.github.com"
	if cfg.APIBase != "" {
		apiBase = cfg.APIBase
	}
	return &githubProvider{
		httpClient: cfg.HTTPClient,
		apiBase:    apiBase,
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     ep,
			RedirectURL:  cfg.CallbackURL,
			Scopes:       scopes,
		},
	}, nil
}

type githubProvider struct {
	oauth      *oauth2.Config
	apiBase    string
	httpClient *http.Client
}

func (p *githubProvider) Name() ProviderName { return ProviderGitHub }
func (p *githubProvider) DisplayName() string { return "GitHub" }

func (p *githubProvider) AuthorizeURL(state, _ string) string {
	// GitHub ignores `nonce`; OIDC providers pick it up in their
	// AuthCodeURL extras.
	return p.oauth.AuthCodeURL(state, oauth2.AccessTypeOnline)
}

func (p *githubProvider) Exchange(ctx context.Context, code, _, _ string) (Claims, error) {
	tok, err := p.oauth.Exchange(p.bindClient(ctx), code)
	if err != nil {
		return Claims{}, fmt.Errorf("github auth: exchange: %w", err)
	}

	user, err := p.fetchUser(ctx, tok)
	if err != nil {
		return Claims{}, err
	}

	// /user only returns email when the user marked it public.
	// /user/emails is private and needs the user:email scope — we
	// pick the primary verified email from there.
	if user.Email == "" {
		email, err := p.fetchPrimaryEmail(ctx, tok)
		if err != nil {
			return Claims{}, err
		}
		user.Email = email
	}
	if user.Email == "" || user.Subject == "" {
		return Claims{}, ErrClaimsMissing
	}
	return user, nil
}

func (p *githubProvider) bindClient(ctx context.Context) context.Context {
	if p.httpClient == nil {
		return ctx
	}
	return context.WithValue(ctx, oauth2.HTTPClient, p.httpClient)
}

type ghUserResponse struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

func (p *githubProvider) fetchUser(ctx context.Context, tok *oauth2.Token) (Claims, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+"/user", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	res, err := p.httpOr().Do(req)
	if err != nil {
		return Claims{}, fmt.Errorf("github auth: /user: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Claims{}, fmt.Errorf("github auth: /user status %d", res.StatusCode)
	}
	var u ghUserResponse
	if err := json.NewDecoder(res.Body).Decode(&u); err != nil {
		return Claims{}, fmt.Errorf("github auth: decode /user: %w", err)
	}
	name := u.Name
	if name == "" {
		name = u.Login
	}
	return Claims{
		Subject:   strconv.FormatInt(u.ID, 10),
		Email:     u.Email,
		Name:      name,
		AvatarURL: u.AvatarURL,
	}, nil
}

type ghEmailResponse struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

func (p *githubProvider) fetchPrimaryEmail(ctx context.Context, tok *oauth2.Token) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+"/user/emails", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	res, err := p.httpOr().Do(req)
	if err != nil {
		return "", fmt.Errorf("github auth: /user/emails: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github auth: /user/emails status %d", res.StatusCode)
	}
	var list []ghEmailResponse
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		return "", fmt.Errorf("github auth: decode /user/emails: %w", err)
	}
	// Prefer the primary+verified pair. Fall back to any verified
	// email so the login doesn't refuse a valid-but-not-primary
	// address on accounts that never set one.
	var fallback string
	for _, e := range list {
		if e.Verified && e.Primary {
			return e.Email, nil
		}
		if e.Verified && fallback == "" {
			fallback = e.Email
		}
	}
	return fallback, nil
}

func (p *githubProvider) httpOr() *http.Client {
	if p.httpClient != nil {
		return p.httpClient
	}
	return http.DefaultClient
}
