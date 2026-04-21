package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrBranchNotFound signals that the configured branch doesn't
// exist on the remote — distinct from transport / auth errors so
// callers (initial-trigger seed path) can surface a specific
// message instead of a generic failure.
var ErrBranchNotFound = errors.New("github: branch not found")

// GetBranchHead resolves the commit SHA at the tip of a branch.
// Used by the trigger-seed path when a never-pushed pipeline needs
// a modification to run against: we ask GitHub for the current
// HEAD of the default branch and feed that into the modification
// row so the run has a revision to check out.
func GetBranchHead(ctx context.Context, httpClient *http.Client, cfg Config, branch string) (string, error) {
	if cfg.Owner == "" || cfg.Repo == "" {
		return "", fmt.Errorf("github: owner and repo are required")
	}
	if branch == "" {
		return "", fmt.Errorf("github: branch is required")
	}
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	u := fmt.Sprintf(
		"%s/repos/%s/%s/branches/%s",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.Owner),
		url.PathEscape(cfg.Repo),
		url.PathEscape(branch),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: request %s: %w", u, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%w: %s/%s@%s", ErrBranchNotFound, cfg.Owner, cfg.Repo, branch)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github: %s returned %d: %s",
			u, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}

	var decoded struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("github: decode branch: %w", err)
	}
	if decoded.Commit.SHA == "" {
		return "", fmt.Errorf("github: branch response missing commit.sha")
	}
	return decoded.Commit.SHA, nil
}
