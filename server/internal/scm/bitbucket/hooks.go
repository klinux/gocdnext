package bitbucket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Hook is the slim view of a Bitbucket repo webhook. UID is
// Bitbucket's stable identifier (a UUID-looking string
// "{abc-123-…}") — unlike GitHub's int64 hook id, so the field
// is string here.
type Hook struct {
	UID string
	URL string
}

type CreateHookInput struct {
	Workspace string
	RepoSlug  string
	URL       string
	Secret    string
	// Description is what shows in Bitbucket's webhook list UI —
	// gocdnext identifies its own hooks by a stable string so a
	// human skimming the list knows what owns them.
	Description string
}

// ListRepoHooks fetches every webhook configured on
// {workspace}/{repo_slug}. Single page (max ~30 hooks is the
// typical ceiling) is enough.
func ListRepoHooks(
	ctx context.Context, httpClient *http.Client,
	cfg Config,
) ([]Hook, error) {
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	u := fmt.Sprintf(
		"%s/repositories/%s/%s/hooks?pagelen=100",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.Workspace),
		url.PathEscape(cfg.RepoSlug),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	setAuth(req, cfg)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bitbucket: list hooks: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bitbucket: list hooks: %d %s",
			resp.StatusCode,
			strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var page struct {
		Values []struct {
			UUID string `json:"uuid"`
			URL  string `json:"url"`
		} `json:"values"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("bitbucket: decode hooks: %w", err)
	}
	out := make([]Hook, 0, len(page.Values))
	for _, h := range page.Values {
		out = append(out, Hook{UID: h.UUID, URL: h.URL})
	}
	return out, nil
}

// CreateRepoHook registers a new repo:push webhook with an HMAC
// secret. Bitbucket stores the secret server-side and uses it to
// sign every delivery with X-Hub-Signature header (the same
// scheme our bitbucket webhook handler verifies).
func CreateRepoHook(
	ctx context.Context, httpClient *http.Client,
	cfg Config, in CreateHookInput,
) (Hook, error) {
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	desc := in.Description
	if desc == "" {
		desc = "gocdnext"
	}
	payload := map[string]any{
		"description":            desc,
		"url":                    in.URL,
		"active":                 true,
		"events":                 []string{"repo:push"},
		"secret":                 in.Secret,
		"secret_set":             true,
		"skip_cert_verification": false,
	}
	body, _ := json.Marshal(payload)
	u := fmt.Sprintf(
		"%s/repositories/%s/%s/hooks",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.Workspace),
		url.PathEscape(cfg.RepoSlug),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return Hook{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req, cfg)
	resp, err := httpClient.Do(req)
	if err != nil {
		return Hook{}, fmt.Errorf("bitbucket: create hook: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Hook{}, fmt.Errorf("bitbucket: create hook: %d %s",
			resp.StatusCode,
			strings.TrimSpace(string(respBody[:min(len(respBody), 200)])))
	}
	var raw struct {
		UUID string `json:"uuid"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return Hook{}, fmt.Errorf("bitbucket: decode created hook: %w", err)
	}
	return Hook{UID: raw.UUID, URL: raw.URL}, nil
}

// UpdateRepoHook PUTs an existing hook so its url + secret match
// whatever we're rotating to.
func UpdateRepoHook(
	ctx context.Context, httpClient *http.Client,
	cfg Config, hookUID string, in CreateHookInput,
) (Hook, error) {
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	desc := in.Description
	if desc == "" {
		desc = "gocdnext"
	}
	payload := map[string]any{
		"description":            desc,
		"url":                    in.URL,
		"active":                 true,
		"events":                 []string{"repo:push"},
		"secret":                 in.Secret,
		"secret_set":             true,
		"skip_cert_verification": false,
	}
	body, _ := json.Marshal(payload)
	u := fmt.Sprintf(
		"%s/repositories/%s/%s/hooks/%s",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.Workspace),
		url.PathEscape(cfg.RepoSlug),
		url.PathEscape(hookUID),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return Hook{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req, cfg)
	resp, err := httpClient.Do(req)
	if err != nil {
		return Hook{}, fmt.Errorf("bitbucket: update hook: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Hook{}, fmt.Errorf("bitbucket: update hook: %d %s",
			resp.StatusCode,
			strings.TrimSpace(string(respBody[:min(len(respBody), 200)])))
	}
	var raw struct {
		UUID string `json:"uuid"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return Hook{}, fmt.Errorf("bitbucket: decode updated hook: %w", err)
	}
	return Hook{UID: raw.UUID, URL: raw.URL}, nil
}

// FindHookForURL matches the github/gitlab helpers — first hook
// with a url starting with prefix (case-insensitive).
func FindHookForURL(hooks []Hook, prefix string) (Hook, bool) {
	p := strings.ToLower(prefix)
	for _, h := range hooks {
		if strings.HasPrefix(strings.ToLower(h.URL), p) {
			return h, true
		}
	}
	return Hook{}, false
}
