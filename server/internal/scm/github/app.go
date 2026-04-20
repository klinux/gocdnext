package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// ErrNoInstallation is returned by InstallationID when the App is not
// installed on the requested repo. Callers typically log an "ask the
// operator to install the App on org/repo" message and move on.
var ErrNoInstallation = errors.New("github: app not installed on repo")

// AppConfig configures a GitHub App authentication client. PrivateKey
// PEM is the block downloaded from the App settings (BEGIN RSA
// PRIVATE KEY or BEGIN PRIVATE KEY). APIBase is optional (default
// https://api.github.com) and lets us point at GitHub Enterprise.
type AppConfig struct {
	AppID         int64
	PrivateKeyPEM []byte
	APIBase       string
	HTTPClient    *http.Client // nil = default
}

// AppClient mints JWTs signed by the App's private key and exchanges
// them for short-lived installation tokens that are scoped to a
// single repo. Tokens are cached per-installation until ~5 minutes
// before expiry so hot paths don't re-hit the API.
type AppClient struct {
	appID      int64
	privateKey *rsa.PrivateKey
	apiBase    string
	httpClient *http.Client
	now        func() time.Time

	mu    sync.Mutex
	cache map[int64]cachedToken
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// NewAppClientFromEnv reads a private key from an inline PEM blob OR
// a file path (mutually exclusive) and returns a client. Returns
// (nil, nil) when appID is 0 — the caller treats that as "App
// disabled" rather than an error, so deployments without a GitHub
// App boot cleanly.
func NewAppClientFromEnv(appID int64, inlinePEM, keyFile, apiBase string) (*AppClient, error) {
	if appID == 0 {
		return nil, nil
	}
	var pemBytes []byte
	switch {
	case inlinePEM != "":
		pemBytes = []byte(inlinePEM)
	case keyFile != "":
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("github: read key file: %w", err)
		}
		pemBytes = b
	default:
		return nil, errors.New("github: GOCDNEXT_GITHUB_APP_PRIVATE_KEY or _FILE must be set when AppID is configured")
	}
	return NewAppClient(AppConfig{
		AppID:         appID,
		PrivateKeyPEM: pemBytes,
		APIBase:       apiBase,
	})
}

// NewAppClient parses the PEM private key and returns a ready client.
// The key must be RS256-capable (any RSA key is fine).
func NewAppClient(cfg AppConfig) (*AppClient, error) {
	if cfg.AppID == 0 {
		return nil, errors.New("github: AppID is required")
	}
	key, err := parseRSAPrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	base := cfg.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &AppClient{
		appID:      cfg.AppID,
		privateKey: key,
		apiBase:    base,
		httpClient: client,
		now:        time.Now,
		cache:      make(map[int64]cachedToken),
	}, nil
}

// InstallationID resolves (owner, repo) to the installation that
// owns the App's permissions on that repo. Returns ErrNoInstallation
// on 404 so the caller can present a useful message.
func (c *AppClient) InstallationID(ctx context.Context, owner, repo string) (int64, error) {
	if owner == "" || repo == "" {
		return 0, errors.New("github: owner and repo are required")
	}
	path := "/repos/" + owner + "/" + repo + "/installation"
	req, err := c.newAppRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("github: installation lookup: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrNoInstallation
	}
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("github: installation lookup returned %s", resp.Status)
	}

	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("github: decode installation: %w", err)
	}
	return out.ID, nil
}

// InstallationToken returns a cached installation token, or mints a
// fresh one when the cached one is missing / near expiry. Installation
// tokens live 1h; we refresh with a 5-minute safety margin so a
// slow caller doesn't end up using an expired one.
func (c *AppClient) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	c.mu.Lock()
	if t, ok := c.cache[installationID]; ok && c.now().Add(5*time.Minute).Before(t.expiresAt) {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()

	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	req, err := c.newAppRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: installation token: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("github: installation token returned %s: %s", resp.Status, string(body))
	}

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("github: decode installation token: %w", err)
	}

	c.mu.Lock()
	c.cache[installationID] = cachedToken{token: out.Token, expiresAt: out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

// DoAsInstallation runs an HTTP request against the API with the
// installation-scoped OAuth-style token attached. Useful when a
// caller already has a *http.Request they built. For convenience the
// Accept + UA headers are set if missing.
func (c *AppClient) DoAsInstallation(ctx context.Context, installationID int64, req *http.Request) (*http.Response, error) {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "gocdnext")
	}
	if req.Header.Get("X-GitHub-Api-Version") == "" {
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return c.httpClient.Do(req.WithContext(ctx))
}

// APIBase exposes the configured base URL so callers can join paths.
func (c *AppClient) APIBase() string { return c.apiBase }

// newAppRequest builds a request authenticated with a short-lived app
// JWT (not an installation token). This is what the App-level
// endpoints (/app/...) require.
func (c *AppClient) newAppRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	jwt, err := c.mintJWT()
	if err != nil {
		return nil, err
	}
	var r io.Reader
	if len(body) > 0 {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("User-Agent", "gocdnext")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
}

// mintJWT produces a GitHub App JWT: header + payload base64url-
// encoded + RS256 signature. Payload carries iat (1 min backdated
// to absorb clock skew), exp (+10 min, GitHub's hard cap), iss.
func (c *AppClient) mintJWT() (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	now := c.now()
	payload, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(c.appID, 10),
	})

	enc := base64.RawURLEncoding
	head := enc.EncodeToString(header)
	body := enc.EncodeToString(payload)
	signingInput := head + "." + body

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github: jwt sign: %w", err)
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}

// parseRSAPrivateKey handles both "RSA PRIVATE KEY" (PKCS#1) and
// "PRIVATE KEY" (PKCS#8) PEM blocks — GitHub currently emits PKCS#1
// but GHE has emitted the other in the past.
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	if len(pemBytes) == 0 {
		return nil, errors.New("github: empty private key")
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github: no PEM block in private key")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("github: pkcs8: %w", err)
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("github: pkcs8 key is not RSA")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("github: unexpected PEM type %q", block.Type)
	}
}
