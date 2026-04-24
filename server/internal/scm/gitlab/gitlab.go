// Package gitlab is the thin GitLab-API client for the drift-
// detection + initial-sync paths. Mirrors the shape of the
// github package — same Config, same Fetch + GetBranchHead entry
// points, same scm.RawFile return type — so the dispatcher in
// configsync can swap implementations by scm.Provider without
// surface-area drift.
//
// GitLab.com + self-hosted Enterprise/CE work the same API; the
// APIBase override picks between them (https://gitlab.com/api/v4
// by default, self-hosted takes https://gitlab.corp/api/v4).
package gitlab

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

// DefaultAPIBase is the SaaS GitLab v4 REST API root. Override
// via Config for self-hosted.
const DefaultAPIBase = "https://gitlab.com/api/v4"

// Config carries the per-call parameters. ProjectPath is the
// namespace/project slug ("org/my-repo") — GitLab's API accepts
// it URL-encoded as the project id, so we never have to resolve
// to a numeric id first.
type Config struct {
	APIBase     string // empty → DefaultAPIBase
	ProjectPath string // e.g. "org/my-repo" (no leading slash, no .git suffix)
	Token       string // Personal Access Token w/ read_api scope
}

// ParseRepoURL extracts the "namespace/project" path from a
// common GitLab URL form. Accepts:
//   - https://gitlab.com/org/repo[.git]
//   - https://gitlab.corp/namespace/sub/repo[.git]   (nested groups)
//   - git@gitlab.com:org/repo[.git]
func ParseRepoURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimSuffix(trimmed, "/")
	trimmed = strings.TrimSuffix(trimmed, ".git")
	if trimmed == "" {
		return "", fmt.Errorf("gitlab: empty url")
	}
	// ssh form: git@host:namespace/repo
	if strings.HasPrefix(trimmed, "git@") {
		after := strings.SplitN(trimmed, ":", 2)
		if len(after) != 2 || after[1] == "" {
			return "", fmt.Errorf("gitlab: invalid ssh url %q", raw)
		}
		return after[1], nil
	}
	// https form: peel off scheme + host
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("gitlab: parse url: %w", err)
	}
	path := strings.TrimPrefix(u.Path, "/")
	if path == "" {
		return "", fmt.Errorf("gitlab: url missing path: %q", raw)
	}
	return path, nil
}

// FetchGocdnextFolder pulls every .yaml/.yml inside `path` at the
// given ref. Empty path defaults to ".gocdnext". Returns
// scm.ErrFolderNotFound when the folder doesn't exist on the
// ref (wrapped so errors.Is matches).
func FetchGocdnextFolder(
	ctx context.Context, httpClient *http.Client, cfg Config,
	ref, path string,
) ([]scm.RawFile, error) {
	if cfg.ProjectPath == "" {
		return nil, fmt.Errorf("gitlab: project path is required")
	}
	if ref == "" {
		return nil, fmt.Errorf("gitlab: ref is required")
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

	// List entries in the folder. GitLab's repository/tree endpoint
	// returns dir entries lazily (pagination via ?per_page=&page=);
	// the .gocdnext folder is tiny in practice, so one page = 100
	// entries is more than enough.
	listURL := fmt.Sprintf(
		"%s/projects/%s/repository/tree?path=%s&ref=%s&per_page=100",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.ProjectPath),
		url.QueryEscape(path),
		url.QueryEscape(ref),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	if cfg.Token != "" {
		req.Header.Set("PRIVATE-TOKEN", cfg.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab: list tree: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s@%s/%s",
			scm.ErrFolderNotFound, cfg.ProjectPath, ref, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitlab: tree %s returned %d: %s",
			listURL, resp.StatusCode,
			strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var entries []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("gitlab: decode tree: %w", err)
	}

	out := make([]scm.RawFile, 0, len(entries))
	for _, e := range entries {
		if e.Type != "blob" {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		content, err := fetchFile(ctx, httpClient, apiBase, cfg, ref, e.Path)
		if err != nil {
			return nil, err
		}
		out = append(out, scm.RawFile{Name: e.Name, Content: content})
	}
	return out, nil
}

// fetchFile downloads one file's content via the repository/files
// endpoint. GitLab returns the content base64-encoded in a JSON
// envelope (encoding: "base64") — we decode and return utf-8.
func fetchFile(
	ctx context.Context, httpClient *http.Client, apiBase string,
	cfg Config, ref, path string,
) (string, error) {
	u := fmt.Sprintf(
		"%s/projects/%s/repository/files/%s?ref=%s",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.ProjectPath),
		url.PathEscape(path),
		url.QueryEscape(ref),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	if cfg.Token != "" {
		req.Header.Set("PRIVATE-TOKEN", cfg.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("gitlab: fetch file %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gitlab: file %s returned %d: %s",
			u, resp.StatusCode,
			strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var decoded struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("gitlab: decode file: %w", err)
	}
	if decoded.Encoding == "base64" {
		raw, err := base64.StdEncoding.DecodeString(decoded.Content)
		if err != nil {
			return "", fmt.Errorf("gitlab: base64 decode %s: %w", path, err)
		}
		return string(raw), nil
	}
	// GitLab can also return text directly (some self-hosted configs).
	return decoded.Content, nil
}

// GetBranchHead resolves a branch to its tip commit SHA. Used by
// the trigger-seed path to feed a "never-pushed" pipeline a
// modification row at HEAD so Run latest has something to run.
func GetBranchHead(
	ctx context.Context, httpClient *http.Client,
	cfg Config, branch string,
) (string, error) {
	if cfg.ProjectPath == "" {
		return "", fmt.Errorf("gitlab: project path is required")
	}
	if branch == "" {
		return "", fmt.Errorf("gitlab: branch is required")
	}
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	u := fmt.Sprintf(
		"%s/projects/%s/repository/branches/%s",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.ProjectPath),
		url.PathEscape(branch),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	if cfg.Token != "" {
		req.Header.Set("PRIVATE-TOKEN", cfg.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("gitlab: branch request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gitlab: branch %s returned %d: %s",
			u, resp.StatusCode,
			strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var decoded struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("gitlab: decode branch: %w", err)
	}
	if decoded.Commit.ID == "" {
		return "", fmt.Errorf("gitlab: branch response missing commit.id")
	}
	return decoded.Commit.ID, nil
}
