package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	commitsPerPage = 100
	// The endpoint serves at most 250 commits for a PR; 10 pages is a safe
	// ceiling that also bounds the work if a provider ever paginates further.
	maxCommitPages = 10
)

type commitEntry struct {
	Commit struct {
		Author struct {
			Date time.Time `json:"date"`
		} `json:"author"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

// FetchPRFirstCommit returns the timestamp of the earliest commit on a pull
// request — the start of the DORA "Coding" stage (first commit → PR opened).
//
// The endpoint's ordering is not contractual, so we paginate the commits and
// take the GLOBAL MIN author date (committer date as fallback — a rebase
// rewrites the committer date). Runs off the webhook hot path, so the extra
// pages cost nothing user-visible. A zero time with a nil error means the PR
// has no commits / no usable date.
func FetchPRFirstCommit(ctx context.Context, httpClient *http.Client, cfg Config, number int) (time.Time, error) {
	if cfg.Owner == "" || cfg.Repo == "" {
		return time.Time{}, fmt.Errorf("github: owner and repo are required")
	}
	if number <= 0 {
		return time.Time{}, fmt.Errorf("github: pr number must be positive, got %d", number)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	base := cfg.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	base = strings.TrimRight(base, "/")

	var earliest time.Time
	for page := 1; page <= maxCommitPages; page++ {
		commits, err := fetchCommitPage(ctx, httpClient, cfg, base, number, page)
		if err != nil {
			return time.Time{}, err
		}
		for _, c := range commits {
			d := c.Commit.Author.Date
			if d.IsZero() {
				d = c.Commit.Committer.Date
			}
			if d.IsZero() {
				continue
			}
			if earliest.IsZero() || d.Before(earliest) {
				earliest = d
			}
		}
		if len(commits) < commitsPerPage {
			break // last (or only) page
		}
	}
	return earliest, nil
}

func fetchCommitPage(ctx context.Context, httpClient *http.Client, cfg Config, base string, number, page int) ([]commitEntry, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/commits?per_page=%d&page=%d",
		base, url.PathEscape(cfg.Owner), url.PathEscape(cfg.Repo), number, commitsPerPage, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: fetch pr commits: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: pr commits status %d", resp.StatusCode)
	}
	var commits []commitEntry
	if err := json.NewDecoder(resp.Body).Decode(&commits); err != nil {
		return nil, fmt.Errorf("github: decode pr commits: %w", err)
	}
	return commits, nil
}
