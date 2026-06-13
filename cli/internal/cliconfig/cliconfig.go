// Package cliconfig stores per-server API tokens for the CLI
// (~/.config/gocdnext/config.json, 0600) and builds http.Clients that
// attach them as Bearer headers. Resolution order: GOCDNEXT_TOKEN env
// (CI/bots) > config file > none (auth-disabled servers keep working).
//
// Tokens never travel through argv — `gocdnext login` reads them from
// stdin or the TTY, the same contract as `secret set`.
package cliconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnvToken overrides any stored token when set; meant for CI and bots
// where a config file is awkward.
const EnvToken = "GOCDNEXT_TOKEN"

type fileConfig struct {
	// Servers maps normalized server URL → API token.
	Servers map[string]serverConfig `json:"servers"`
}

type serverConfig struct {
	Token string `json:"token"`
}

// Path returns the config file location, honoring XDG_CONFIG_HOME.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(dir, "gocdnext", "config.json"), nil
}

// normalizeServer collapses trailing slashes so login/lookup agree on
// the map key regardless of how the user typed the URL.
func normalizeServer(serverURL string) string {
	return strings.TrimRight(strings.TrimSpace(serverURL), "/")
}

func load() (fileConfig, error) {
	cfg := fileConfig{Servers: map[string]serverConfig{}}
	path, err := Path()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]serverConfig{}
	}
	return cfg, nil
}

func save(cfg fileConfig) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	// Write-then-rename keeps the credential file from ever being
	// observable half-written or with loose permissions.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// ResolveToken returns the token for serverURL, or "" when none is
// configured. The GOCDNEXT_TOKEN env var wins over the config file.
func ResolveToken(serverURL string) string {
	if tok := os.Getenv(EnvToken); tok != "" {
		return tok
	}
	cfg, err := load()
	if err != nil {
		// A corrupt config must not brick every command; the user can
		// re-login. Unauthenticated calls fail loudly with 401 anyway.
		fmt.Fprintf(os.Stderr, "warning: %v (run `gocdnext login` to rewrite it)\n", err)
		return ""
	}
	return cfg.Servers[normalizeServer(serverURL)].Token
}

// SetToken stores token for serverURL in the config file (0600).
func SetToken(serverURL, token string) error {
	cfg, err := load()
	if err != nil {
		return err
	}
	cfg.Servers[normalizeServer(serverURL)] = serverConfig{Token: token}
	return save(cfg)
}

// DeleteToken removes the stored token for serverURL, if any.
func DeleteToken(serverURL string) error {
	cfg, err := load()
	if err != nil {
		return err
	}
	delete(cfg.Servers, normalizeServer(serverURL))
	return save(cfg)
}

type bearerTransport struct {
	token string
	// scheme+host of the configured server. The header is bound to
	// this origin: http.Client's own Authorization-stripping on
	// cross-domain redirects only covers headers set on the INITIAL
	// request — a transport re-adds them after every redirect hop,
	// so it must enforce the boundary itself or a 302 from the
	// server would hand the token to an arbitrary third party.
	scheme string
	host   string
	base   http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.scheme || req.URL.Host != t.host {
		return t.base.RoundTrip(req)
	}
	// Per RoundTripper contract the request must not be mutated.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// HTTPClient returns a client that sends the resolved token for
// serverURL as a Bearer header on requests to that origin ONLY —
// redirects or absolute URLs pointing elsewhere go out without it.
// With no token it behaves like a plain client. timeout <= 0 means 30s.
func HTTPClient(serverURL string, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	tok := ResolveToken(serverURL)
	if tok == "" {
		return client
	}
	origin, err := url.Parse(normalizeServer(serverURL))
	if err != nil || origin.Scheme == "" || origin.Host == "" {
		// Unparsable server URL: fail safe — no token anywhere. The
		// request itself will fail loudly against the same bad URL.
		return client
	}
	client.Transport = &bearerTransport{
		token:  tok,
		scheme: origin.Scheme,
		host:   origin.Host,
		base:   http.DefaultTransport,
	}
	return client
}

// ValidateToken checks token against GET /api/v1/me and returns the
// authenticated identity (email or name) on success.
func ValidateToken(ctx context.Context, serverURL, token string) (string, error) {
	url := normalizeServer(serverURL) + "/api/v1/me"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach %s: %w", serverURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", errors.New("token rejected by the server (expired or revoked?)")
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("server returned %s for /api/v1/me", resp.Status)
	}
	// Server shape: {"user": {"email": ..., "name": ..., ...}}
	// (authapi handler.Me wraps store.User).
	var me struct {
		User struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&me); err != nil {
		return "", fmt.Errorf("decode /api/v1/me response: %w", err)
	}
	who := me.User.Email
	if who == "" {
		who = me.User.Name
	}
	if who == "" {
		who = "authenticated user"
	}
	return who, nil
}
