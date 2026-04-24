package gitlab

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

// Hook is the slim view of a project webhook the auto-register
// path cares about: just enough to match-or-create idempotently.
// GitLab's hook API carries many other toggles (issue events,
// merge events, confidential events, ssl verification flags); we
// ignore them because gocdnext only consumes push events.
type Hook struct {
	ID  int64
	URL string
}

// CreateHookInput carries the fields we actually set. Events is
// always "push" for gocdnext — merge_request + tag_push land on
// the roadmap only if a pipeline trigger wires them. Secret goes
// into GitLab's "token" field, which is the plaintext they echo
// back as X-Gitlab-Token on delivery (no HMAC signing in their
// model — the token IS the shared-secret check).
type CreateHookInput struct {
	ProjectPath string // "namespace/project", URL-encoded by caller
	URL         string
	Secret      string
}

// ListProjectHooks fetches every webhook on the project. N is
// normally small (a dozen or two tops), so a single page of 100
// per_page covers it. Returns an error on 404 so a wrong
// ProjectPath surfaces loud.
func ListProjectHooks(
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
		"%s/projects/%s/hooks?per_page=100",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.ProjectPath),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if cfg.Token != "" {
		req.Header.Set("PRIVATE-TOKEN", cfg.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab: list hooks: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitlab: list hooks %s: %d %s",
			u, resp.StatusCode,
			strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var raw []struct {
		ID  int64  `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("gitlab: decode hooks: %w", err)
	}
	out := make([]Hook, 0, len(raw))
	for _, h := range raw {
		out = append(out, Hook{ID: h.ID, URL: h.URL})
	}
	return out, nil
}

// CreateProjectHook registers a new push-events webhook on the
// project. Returns the GitLab hook id so callers can store /
// update it later. Enables SSL verification — if the target is
// self-signed, the operator fixes the cert, we don't downgrade.
func CreateProjectHook(
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
	payload := map[string]any{
		"url":                      in.URL,
		"token":                    in.Secret,
		"push_events":              true,
		"enable_ssl_verification":  true,
	}
	body, _ := json.Marshal(payload)
	u := fmt.Sprintf(
		"%s/projects/%s/hooks",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.ProjectPath),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return Hook{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("PRIVATE-TOKEN", cfg.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Hook{}, fmt.Errorf("gitlab: create hook: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Hook{}, fmt.Errorf("gitlab: create hook: %d %s",
			resp.StatusCode,
			strings.TrimSpace(string(respBody[:min(len(respBody), 200)])))
	}
	var raw struct {
		ID  int64  `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return Hook{}, fmt.Errorf("gitlab: decode created hook: %w", err)
	}
	return Hook{ID: raw.ID, URL: raw.URL}, nil
}

// UpdateProjectHook PUTs a hook so its URL + token match the
// current ones. Called by the rotate-webhook path to keep the
// provider-side token in sync with the sealed one we re-mint in
// the DB.
func UpdateProjectHook(
	ctx context.Context, httpClient *http.Client,
	cfg Config, hookID int64, in CreateHookInput,
) (Hook, error) {
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	payload := map[string]any{
		"url":                     in.URL,
		"token":                   in.Secret,
		"push_events":             true,
		"enable_ssl_verification": true,
	}
	body, _ := json.Marshal(payload)
	u := fmt.Sprintf(
		"%s/projects/%s/hooks/%d",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.ProjectPath),
		hookID,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return Hook{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("PRIVATE-TOKEN", cfg.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Hook{}, fmt.Errorf("gitlab: update hook: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Hook{}, fmt.Errorf("gitlab: update hook: %d %s",
			resp.StatusCode,
			strings.TrimSpace(string(respBody[:min(len(respBody), 200)])))
	}
	var raw struct {
		ID  int64  `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return Hook{}, fmt.Errorf("gitlab: decode updated hook: %w", err)
	}
	return Hook{ID: raw.ID, URL: raw.URL}, nil
}

// FindHookForURL returns the first hook whose url starts with
// prefix (case-insensitive), matching the github helper's
// semantic so autoregister's idempotency check is identical
// across providers.
func FindHookForURL(hooks []Hook, prefix string) (Hook, bool) {
	p := strings.ToLower(prefix)
	for _, h := range hooks {
		if strings.HasPrefix(strings.ToLower(h.URL), p) {
			return h, true
		}
	}
	return Hook{}, false
}
