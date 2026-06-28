package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// FetchPRFirstCommit returns the timestamp of the earliest commit on a pull
// request — the start of the DORA "Coding" stage (first commit → PR opened).
//
// GitHub's commits endpoint returns commits oldest-first, so per_page=1 yields
// the first commit in one cheap call. We prefer the author date (when the work
// was written) over the committer date (which a rebase rewrites). A zero time
// with a nil error means the PR has no commits / no usable date.
func FetchPRFirstCommit(ctx context.Context, httpClient *http.Client, cfg Config, number int) (time.Time, error) {
	if cfg.Owner == "" || cfg.Repo == "" {
		return time.Time{}, fmt.Errorf("github: owner and repo are required")
	}
	if number <= 0 {
		return time.Time{}, fmt.Errorf("github: pr number must be positive, got %d", number)
	}
	apiBase := cfg.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/commits?per_page=1",
		apiBase, cfg.Owner, cfg.Repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("github: fetch pr commits: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("github: pr commits status %d", resp.StatusCode)
	}

	var commits []struct {
		Commit struct {
			Author struct {
				Date time.Time `json:"date"`
			} `json:"author"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commits); err != nil {
		return time.Time{}, fmt.Errorf("github: decode pr commits: %w", err)
	}
	if len(commits) == 0 {
		return time.Time{}, nil
	}
	if d := commits[0].Commit.Author.Date; !d.IsZero() {
		return d, nil
	}
	return commits[0].Commit.Committer.Date, nil
}
