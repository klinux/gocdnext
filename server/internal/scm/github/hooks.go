package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Hook is the subset of a GitHub repo webhook we care about when
// deciding "is a gocdnext hook already installed here?". The full
// shape is rich (insecure_ssl, ping, last response, etc.); we only
// need the ID + events + the config.url for idempotency checks.
type Hook struct {
	ID     int64
	Active bool
	Events []string
	Config HookConfig
}

type HookConfig struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type,omitempty"`
	Secret      string `json:"secret,omitempty"`
	InsecureSSL string `json:"insecure_ssl,omitempty"`
}

type CreateHookInput struct {
	Owner  string
	Repo   string
	URL    string
	Secret string
	Events []string // default: ["push", "pull_request"]
}

// ListRepoHooks returns every webhook configured on (owner/repo),
// authenticated as the installation. Used to detect an existing
// gocdnext hook before creating a new one.
func (c *AppClient) ListRepoHooks(ctx context.Context, installationID int64, owner, repo string) ([]Hook, error) {
	req, err := http.NewRequest(http.MethodGet,
		c.apiBase+"/repos/"+owner+"/"+repo+"/hooks?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.DoAsInstallation(ctx, installationID, req)
	if err != nil {
		return nil, fmt.Errorf("github: list hooks: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github: list hooks returned %s: %s", resp.Status, body)
	}
	var raw []struct {
		ID     int64      `json:"id"`
		Active bool       `json:"active"`
		Events []string   `json:"events"`
		Config HookConfig `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("github: decode hooks: %w", err)
	}
	out := make([]Hook, 0, len(raw))
	for _, h := range raw {
		out = append(out, Hook{
			ID: h.ID, Active: h.Active, Events: h.Events, Config: h.Config,
		})
	}
	return out, nil
}

// CreateRepoHook registers a new webhook. Secret is the shared HMAC
// the server will use to verify future deliveries. Events defaults to
// push + pull_request (covering both trigger paths gocdnext
// supports). InsecureSSL is forced off — if your GitHub Enterprise
// uses self-signed certs, GHE admins fix the cert, we don't work
// around it.
func (c *AppClient) CreateRepoHook(ctx context.Context, installationID int64, in CreateHookInput) (Hook, error) {
	events := in.Events
	if len(events) == 0 {
		events = []string{"push", "pull_request"}
	}
	payload := map[string]any{
		"name":   "web",
		"active": true,
		"events": events,
		"config": map[string]any{
			"url":          in.URL,
			"content_type": "json",
			"secret":       in.Secret,
			"insecure_ssl": "0",
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost,
		c.apiBase+"/repos/"+in.Owner+"/"+in.Repo+"/hooks", bytes.NewReader(body))
	if err != nil {
		return Hook{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.DoAsInstallation(ctx, installationID, req)
	if err != nil {
		return Hook{}, fmt.Errorf("github: create hook: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		rb, _ := io.ReadAll(resp.Body)
		return Hook{}, fmt.Errorf("github: create hook returned %s: %s", resp.Status, rb)
	}
	var raw struct {
		ID     int64      `json:"id"`
		Active bool       `json:"active"`
		Events []string   `json:"events"`
		Config HookConfig `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return Hook{}, fmt.Errorf("github: decode created hook: %w", err)
	}
	return Hook{ID: raw.ID, Active: raw.Active, Events: raw.Events, Config: raw.Config}, nil
}

// UpdateRepoHook PATCHes an existing hook so its config URL +
// secret match whatever we're rotating to. Events default to
// push + pull_request (same as create) — passing nil rotates
// only the config pieces GitHub lets us change. Used by the
// rotate-webhook endpoint to keep the provider-side secret in
// sync with the sealed one we just re-minted in the DB.
func (c *AppClient) UpdateRepoHook(ctx context.Context, installationID, hookID int64, in CreateHookInput) (Hook, error) {
	events := in.Events
	if len(events) == 0 {
		events = []string{"push", "pull_request"}
	}
	payload := map[string]any{
		"active": true,
		"events": events,
		"config": map[string]any{
			"url":          in.URL,
			"content_type": "json",
			"secret":       in.Secret,
			"insecure_ssl": "0",
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPatch,
		fmt.Sprintf("%s/repos/%s/%s/hooks/%d", c.apiBase, in.Owner, in.Repo, hookID),
		bytes.NewReader(body))
	if err != nil {
		return Hook{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.DoAsInstallation(ctx, installationID, req)
	if err != nil {
		return Hook{}, fmt.Errorf("github: update hook: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		rb, _ := io.ReadAll(resp.Body)
		return Hook{}, fmt.Errorf("github: update hook returned %s: %s", resp.Status, rb)
	}
	var raw struct {
		ID     int64      `json:"id"`
		Active bool       `json:"active"`
		Events []string   `json:"events"`
		Config HookConfig `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return Hook{}, fmt.Errorf("github: decode updated hook: %w", err)
	}
	return Hook{ID: raw.ID, Active: raw.Active, Events: raw.Events, Config: raw.Config}, nil
}

// FindHookForURL returns the first hook whose config.url matches a
// prefix (typically the gocdnext public base). Used for idempotency —
// we don't want to create a second hook alongside an existing one on
// re-apply. Match is case-insensitive on scheme + host.
func FindHookForURL(hooks []Hook, prefix string) (Hook, bool) {
	p := strings.ToLower(prefix)
	for _, h := range hooks {
		if strings.HasPrefix(strings.ToLower(h.Config.URL), p) {
			return h, true
		}
	}
	return Hook{}, false
}
