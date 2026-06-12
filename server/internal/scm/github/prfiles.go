package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// prFilesMaxPages caps the PR files walk at 30 pages × 100 files =
// 3000 entries — the same ceiling the GitHub UI applies to PR diffs.
// Past the cap the list is INCOMPLETE and the caller must fail open;
// a 3000-file PR is a vendoring/codegen sweep that should run CI
// anyway.
const prFilesMaxPages = 30

// FetchPRFiles lists the changed files of a pull request via
// GET /repos/{owner}/{repo}/pulls/{number}/files, paginated.
// `complete=false` flags a truncated walk (pagination cap reached);
// errors are returned for transport/HTTP failures so the caller can
// log and fail open.
//
// Renamed files contribute BOTH sides: `filename` (new path) and
// `previous_filename` (old path) — a rename out of a watched
// directory still matches the glob that watched it.
func FetchPRFiles(ctx context.Context, httpClient *http.Client, cfg Config, number int) (files []string, complete bool, err error) {
	if cfg.Owner == "" || cfg.Repo == "" {
		return nil, false, fmt.Errorf("github: owner and repo are required")
	}
	if number <= 0 {
		return nil, false, fmt.Errorf("github: pr number must be positive, got %d", number)
	}
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	seen := make(map[string]struct{})
	add := func(f string) {
		if f == "" {
			return
		}
		if _, dup := seen[f]; !dup {
			seen[f] = struct{}{}
			files = append(files, f)
		}
	}

	for page := 1; page <= prFilesMaxPages; page++ {
		pageURL := fmt.Sprintf(
			"%s/repos/%s/%s/pulls/%d/files?per_page=100&page=%d",
			strings.TrimRight(apiBase, "/"),
			url.PathEscape(cfg.Owner),
			url.PathEscape(cfg.Repo),
			number, page,
		)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return nil, false, fmt.Errorf("github: build pr files request: %w", err)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, false, fmt.Errorf("github: pr files request: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, false, fmt.Errorf("github: read pr files response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, false, fmt.Errorf("github: pr files returned %d: %s",
				resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
		}
		var entries []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		if err := json.Unmarshal(body, &entries); err != nil {
			return nil, false, fmt.Errorf("github: decode pr files: %w", err)
		}
		for _, e := range entries {
			add(e.Filename)
			add(e.PreviousFilename)
		}
		if len(entries) < 100 {
			return files, true, nil
		}
	}
	// Walked every allowed page and the last one was still full —
	// there may be more files. Partial set, caller fails open.
	return files, false, nil
}
