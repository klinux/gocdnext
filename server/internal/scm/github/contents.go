// Package github is the thin GitHub-API client the drift-detection path
// uses to re-read `.gocdnext/` after a push. Unauthenticated requests work
// for public repos but are rate-limited; a PAT (from scm_sources.auth_ref)
// gets a higher ceiling and lets us read private ones.
package github

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

// DefaultAPIBase is the GitHub.com v3 REST API root. Override via Config for
// GitHub Enterprise.
const DefaultAPIBase = "https://api.github.com"

// ErrFolderNotFound is re-exported for backwards compat with
// callers that imported it from this package before the shared
// scm package existed. New code should use scm.ErrFolderNotFound
// directly — they compare equal via errors.Is.
var ErrFolderNotFound = scm.ErrFolderNotFound

// Config wires one call to the Contents API. Token is optional.
type Config struct {
	APIBase string // empty → DefaultAPIBase
	Owner   string
	Repo    string
	Token   string // personal access token; empty means unauthenticated
}

// RawFile is an alias for the provider-agnostic scm.RawFile so
// existing call sites keep compiling after the type moved.
type RawFile = scm.RawFile

// ParseRepoURL extracts (owner, repo) from a common git URL. It accepts
// https://github.com/<owner>/<repo>[.git] and git@github.com:<owner>/<repo>.
func ParseRepoURL(raw string) (owner, repo string, err error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimSuffix(trimmed, "/")
	trimmed = strings.TrimSuffix(trimmed, ".git")
	if trimmed == "" {
		return "", "", fmt.Errorf("github: empty url")
	}

	// ssh form: git@github.com:owner/repo
	if strings.HasPrefix(trimmed, "git@") {
		after := strings.SplitN(trimmed, ":", 2)
		if len(after) != 2 {
			return "", "", fmt.Errorf("github: cannot parse ssh url %q", raw)
		}
		parts := strings.SplitN(after[1], "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("github: ssh url missing owner/repo: %q", raw)
		}
		return parts[0], parts[1], nil
	}

	u, parseErr := url.Parse(trimmed)
	if parseErr != nil {
		return "", "", fmt.Errorf("github: parse url: %w", parseErr)
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segments) < 2 || segments[0] == "" || segments[1] == "" {
		return "", "", fmt.Errorf("github: url missing owner/repo: %q", raw)
	}
	return segments[0], segments[1], nil
}

// FetchGocdnextFolder lists every `*.yaml`/`*.yml` file directly
// inside the repo's configured pipeline folder at the given ref
// and returns their content. Non-YAML entries and nested
// directories are ignored — the config folder is a flat
// file-per-pipeline convention.
//
// configPath is the repo-relative folder (e.g. ".gocdnext",
// ".woodpecker", "apps/api/.gocdnext"). Empty → ".gocdnext" for
// backwards-compat with older callers. When the path ends in
// .yaml / .yml it's treated as a single-file config (GitLab-CI
// style) and a single RawFile is returned.
func FetchGocdnextFolder(ctx context.Context, httpClient *http.Client, cfg Config, ref, configPath string) ([]RawFile, error) {
	if cfg.Owner == "" || cfg.Repo == "" {
		return nil, fmt.Errorf("github: owner and repo are required")
	}
	if configPath == "" {
		configPath = ".gocdnext"
	}
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	contentsURL := fmt.Sprintf(
		"%s/repos/%s/%s/contents/%s",
		strings.TrimRight(apiBase, "/"),
		url.PathEscape(cfg.Owner),
		url.PathEscape(cfg.Repo),
		escapePath(configPath),
	)
	if ref != "" {
		contentsURL += "?ref=" + url.QueryEscape(ref)
	}

	if isSingleFileConfigPath(configPath) {
		// Single-file mode: contents API returns one object, not
		// an array. Decode as a single contentEntry and wrap it
		// in a one-element slice so the caller doesn't care.
		entry, err := fetchSingleContent(ctx, httpClient, cfg.Token, contentsURL)
		if err != nil {
			return nil, err
		}
		text, err := materialize(ctx, httpClient, cfg.Token, entry)
		if err != nil {
			return nil, err
		}
		return []RawFile{{Name: entry.Name, Content: text}}, nil
	}

	entries, err := fetchContents(ctx, httpClient, cfg.Token, contentsURL)
	if err != nil {
		return nil, err
	}

	out := make([]RawFile, 0, len(entries))
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		switch filepath.Ext(e.Name) {
		case ".yaml", ".yml":
		default:
			continue
		}
		text, err := materialize(ctx, httpClient, cfg.Token, e)
		if err != nil {
			return nil, fmt.Errorf("github: fetch %s: %w", e.Name, err)
		}
		out = append(out, RawFile{Name: e.Name, Content: text})
	}
	return out, nil
}

// materialize returns the plaintext of an inlined content entry,
// falling back to the download_url when GitHub stubbed the blob
// (>1 MiB). Extracted so folder and single-file paths share it.
func materialize(ctx context.Context, client *http.Client, token string, e contentEntry) (string, error) {
	text, err := decodeInlineContent(e)
	if err == nil {
		return text, nil
	}
	return fetchRaw(ctx, client, token, e.DownloadURL)
}

func isSingleFileConfigPath(path string) bool {
	switch filepath.Ext(path) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

type contentEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"` // "file" | "dir" | "symlink" | "submodule"
	Size        int64  `json:"size"`
	Encoding    string `json:"encoding"`
	Content     string `json:"content"`
	DownloadURL string `json:"download_url"`
}

// fetchSingleContent expects the GitHub contents API to return a
// single file object (single-file config_path mode). The same
// endpoint returns an array for folders and an object for files,
// so we decode into the right shape based on the caller's intent.
func fetchSingleContent(ctx context.Context, client *http.Client, token, u string) (contentEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return contentEntry{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return contentEntry{}, fmt.Errorf("github: request %s: %w", u, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return contentEntry{}, fmt.Errorf("%w: %s", ErrFolderNotFound, u)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return contentEntry{}, fmt.Errorf("github: %s returned %d: %s",
			u, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}

	var entry contentEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return contentEntry{}, fmt.Errorf("github: decode single file: %w", err)
	}
	if entry.Type != "file" {
		return contentEntry{}, fmt.Errorf("github: %s is not a file (type=%q)", u, entry.Type)
	}
	return entry, nil
}

func fetchContents(ctx context.Context, client *http.Client, token, u string) ([]contentEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: request %s: %w", u, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrFolderNotFound, u)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github: %s returned %d: %s",
			u, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}

	var entries []contentEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("github: decode contents: %w", err)
	}
	return entries, nil
}

func decodeInlineContent(e contentEntry) (string, error) {
	if e.Encoding != "base64" || e.Content == "" {
		return "", fmt.Errorf("content not inlined (encoding=%q size=%d)", e.Encoding, e.Size)
	}
	// GitHub wraps base64 at 60 chars — the stdlib decoder tolerates whitespace
	// but we keep the string tidy for the error-case log.
	cleaned := strings.Join(strings.Fields(e.Content), "")
	b, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	return string(b), nil
}

func fetchRaw(ctx context.Context, client *http.Client, token, rawURL string) (string, error) {
	if rawURL == "" {
		return "", fmt.Errorf("empty download_url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: raw %s returned %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// escapePath URL-escapes each segment of a slash-delimited path
// individually — url.PathEscape would turn every slash into %2F,
// which GitHub's contents API rejects. Supports nested paths
// like "apps/api/.gocdnext" without mangling the separators.
func escapePath(p string) string {
	segments := strings.Split(strings.Trim(p, "/"), "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/")
}
