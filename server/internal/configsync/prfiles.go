package configsync

import (
	"context"

	gh "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// PRFiles adapts the MultiFetcher's credential machinery to the
// webhook handler's PRFilesFetcher port: given the scm_source of a
// pull_request delivery, list the PR's changed files so `when.paths`
// can filter pipelines. The CONTRACT is fail-open: any miss —
// unsupported provider, URL parse error, API failure, pagination cap
// — returns known=false and the caller runs the pipeline.
//
// v1 supports GitHub (files API + PAT/App credential resolution).
// GitLab MRs and Bitbucket PRs return known=false with a log line —
// paths-gated pipelines on those providers run on every PR until the
// adapters land (mirrors how #11/#12 grew provider support).
type PRFiles struct {
	*MultiFetcher
}

// PRChangedFiles implements webhook.PRFilesFetcher.
func (p *PRFiles) PRChangedFiles(ctx context.Context, source store.SCMSource, number int) ([]string, bool) {
	switch source.Provider {
	case "github":
		owner, repo, err := gh.ParseRepoURL(source.URL)
		if err != nil {
			p.logWarn("prfiles: parse github url", "url", source.URL, "err", err)
			return nil, false
		}
		authRef, apiBase := p.resolveGitHub(ctx, source, owner, repo)
		files, complete, err := gh.FetchPRFiles(ctx, p.client(), gh.Config{
			APIBase: apiBase,
			Owner:   owner,
			Repo:    repo,
			Token:   authRef,
		}, number)
		if err != nil {
			p.logWarn("prfiles: github files api failed — when.paths fails open",
				"owner", owner, "repo", repo, "pr", number, "err", err)
			return nil, false
		}
		return files, complete
	default:
		p.logWarn("prfiles: provider has no PR file-listing adapter yet — when.paths fails open",
			"provider", source.Provider)
		return nil, false
	}
}

func (p *PRFiles) logWarn(msg string, args ...any) {
	if p.MultiFetcher != nil && p.Logger != nil {
		p.Logger.Warn(msg, args...)
	}
}
