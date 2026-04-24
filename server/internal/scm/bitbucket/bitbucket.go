// Package bitbucket is the thin Bitbucket Cloud API client for
// the drift-detection + initial-sync paths. Mirrors the shape of
// github + gitlab — Config + Fetch + GetBranchHead — so
// configsync can dispatch by scm.Provider.
//
// Bitbucket Server (the self-hosted product) has a different API
// surface and isn't covered here; add a sibling "bitbucketserver"
// package when someone needs it.
package bitbucket

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/scm"
)

// DefaultAPIBase is Bitbucket Cloud's v2 REST API root.
const DefaultAPIBase = "https://api.bitbucket.org/2.0"

// Config carries per-call parameters. Username + AppPassword is
// the canonical Bitbucket Cloud auth; AppPassword is created at
// account level with "repositories:read" scope and passed as
// HTTP Basic. Token (OAuth access token) is the alternative —
// set Token and leave Username empty and we send Bearer.
type Config struct {
	APIBase     string
	Workspace   string // e.g. "acme"
	RepoSlug    string // e.g. "my-service"
	Username    string
	AppPassword string
	Token       string // Bearer token; wins over Username/AppPassword when set
}

// ParseRepoURL splits a Bitbucket URL into (workspace, repo_slug).
// Accepts:
//   - https://bitbucket.org/workspace/repo[.git]
//   - git@bitbucket.org:workspace/repo[.git]
func ParseRepoURL(raw string) (workspace, repoSlug string, err error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimSuffix(trimmed, "/")
	trimmed = strings.TrimSuffix(trimmed, ".git")
	if trimmed == "" {
		return "", "", fmt.Errorf("bitbucket: empty url")
	}
	if strings.HasPrefix(trimmed, "git@") {
		after := strings.SplitN(trimmed, ":", 2)
		if len(after) != 2 {
			return "", "", fmt.Errorf("bitbucket: invalid ssh url %q", raw)
		}
		trimmed = after[1]
	} else {
		u, err := url.Parse(trimmed)
		if err != nil {
			return "", "", fmt.Errorf("bitbucket: parse url: %w", err)
		}
		trimmed = strings.TrimPrefix(u.Path, "/")
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("bitbucket: expected workspace/repo, got %q", raw)
	}
	return parts[0], parts[1], nil
}

// FetchGocdnextFolder lists the config folder at ref and
// downloads each .yaml/.yml blob. Bitbucket's /src endpoint
// returns either a directory listing (JSON with values[]) or the
// raw file contents depending on the target — we hit it first in
// directory mode, then per-file in raw mode.
func FetchGocdnextFolder(
	ctx context.Context, httpClient *http.Client, cfg Config,
	ref, path string,
) ([]scm.RawFile, error) {
	if cfg.Workspace == "" || cfg.RepoSlug == "" {
		return nil, fmt.Errorf("bitbucket: workspace + repo_slug are required")
	}
	if ref == "" {
		return nil, fmt.Errorf("bitbucket: ref is required")
	}
	if path == "" {
		path = ".gocdnext"
	}
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	listURL := fmt.Sprintf(
		"%s/repositories/%s/%s/src/%s/%s/?pagelen=100&format=meta",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.Workspace),
		url.PathEscape(cfg.RepoSlug),
		url.PathEscape(ref),
		strings.TrimPrefix(path, "/"),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	setAuth(req, cfg)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bitbucket: list src: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s/%s@%s/%s",
			scm.ErrFolderNotFound, cfg.Workspace, cfg.RepoSlug, ref, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bitbucket: src %s returned %d: %s",
			listURL, resp.StatusCode,
			strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var listing struct {
		Values []struct {
			Path string `json:"path"`
			Type string `json:"type"` // "commit_file" | "commit_directory"
		} `json:"values"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("bitbucket: decode listing: %w", err)
	}

	out := make([]scm.RawFile, 0, len(listing.Values))
	for _, v := range listing.Values {
		if v.Type != "commit_file" {
			continue
		}
		name := filepath.Base(v.Path)
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		content, err := fetchFile(ctx, httpClient, apiBase, cfg, ref, v.Path)
		if err != nil {
			return nil, err
		}
		out = append(out, scm.RawFile{Name: name, Content: content})
	}
	return out, nil
}

// fetchFile pulls one file via the /src endpoint in raw mode.
// Bitbucket returns the bytes directly (no base64 envelope),
// unlike GitHub/GitLab.
func fetchFile(
	ctx context.Context, httpClient *http.Client, apiBase string,
	cfg Config, ref, path string,
) (string, error) {
	u := fmt.Sprintf(
		"%s/repositories/%s/%s/src/%s/%s",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.Workspace),
		url.PathEscape(cfg.RepoSlug),
		url.PathEscape(ref),
		strings.TrimPrefix(path, "/"),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	setAuth(req, cfg)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("bitbucket: fetch file %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("bitbucket: file %s returned %d: %s",
			u, resp.StatusCode,
			strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	return string(body), nil
}

// GetBranchHead resolves a branch to its tip commit SHA.
func GetBranchHead(
	ctx context.Context, httpClient *http.Client,
	cfg Config, branch string,
) (string, error) {
	if cfg.Workspace == "" || cfg.RepoSlug == "" {
		return "", fmt.Errorf("bitbucket: workspace + repo_slug are required")
	}
	if branch == "" {
		return "", fmt.Errorf("bitbucket: branch is required")
	}
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	u := fmt.Sprintf(
		"%s/repositories/%s/%s/refs/branches/%s",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.Workspace),
		url.PathEscape(cfg.RepoSlug),
		url.PathEscape(branch),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	setAuth(req, cfg)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("bitbucket: branch request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("bitbucket: branch %s returned %d: %s",
			u, resp.StatusCode,
			strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var decoded struct {
		Target struct {
			Hash string `json:"hash"`
		} `json:"target"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("bitbucket: decode branch: %w", err)
	}
	if decoded.Target.Hash == "" {
		return "", fmt.Errorf("bitbucket: branch response missing target.hash")
	}
	return decoded.Target.Hash, nil
}

// setAuth picks Bearer vs Basic based on Config. Token wins when
// set (OAuth-style deployments); otherwise fall back to App
// Password over Basic. Empty auth is allowed for public repos.
func setAuth(req *http.Request, cfg Config) {
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		return
	}
	if cfg.Username != "" && cfg.AppPassword != "" {
		creds := cfg.Username + ":" + cfg.AppPassword
		req.Header.Set("Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
	}
}
