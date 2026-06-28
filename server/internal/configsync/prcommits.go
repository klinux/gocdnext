package configsync

import (
	"context"
	"time"

	gh "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// PRCommits adapts the MultiFetcher's credential machinery to the webhook
// handler's PRCommitsFetcher port: given the scm_source of a pull_request, fetch
// the PR's first commit timestamp (the DORA "Coding" stage start). Fail-soft:
// any miss — unsupported provider, URL parse error, API failure, no commits —
// returns ok=false and the lifecycle row simply has no first_commit_at.
//
// v1 supports GitHub only; GitLab/Bitbucket return ok=false (tracked in #123).
type PRCommits struct {
	*MultiFetcher
}

// PRFirstCommitAt implements webhook.PRCommitsFetcher.
func (p *PRCommits) PRFirstCommitAt(ctx context.Context, source store.SCMSource, number int) (time.Time, bool) {
	if source.Provider != "github" {
		return time.Time{}, false
	}
	owner, repo, err := gh.ParseRepoURL(source.URL)
	if err != nil {
		p.logWarn("prcommits: parse github url", "url", source.URL, "err", err)
		return time.Time{}, false
	}
	authRef, apiBase := p.resolveGitHub(ctx, source, owner, repo)
	at, err := gh.FetchPRFirstCommit(ctx, p.client(), gh.Config{
		APIBase: apiBase,
		Owner:   owner,
		Repo:    repo,
		Token:   authRef,
	}, number)
	if err != nil {
		p.logWarn("prcommits: github commits api failed — Coding stage skipped",
			"owner", owner, "repo", repo, "pr", number, "err", err)
		return time.Time{}, false
	}
	if at.IsZero() {
		return time.Time{}, false
	}
	return at, true
}

func (p *PRCommits) logWarn(msg string, args ...any) {
	if p.MultiFetcher != nil && p.Logger != nil {
		p.Logger.Warn(msg, args...)
	}
}
