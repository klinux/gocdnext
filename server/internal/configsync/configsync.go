// Package configsync is the shared "read `.gocdnext/` from a repo
// and turn it into pipeline definitions" layer used by both the
// webhook push/drift path and the project-apply handler.
//
// The webhook path uses it to re-read at the push's revision so
// the live config tracks HEAD. The project-apply path uses it at
// bind time so a project shows its pipelines the moment the scm
// source is registered, without having to wait for a push.
//
// Moving these types out of webhook decouples the projects API
// handler from the webhook package — otherwise api/projects would
// have to import webhook, which inverts the layering (webhook is
// a transport adapter; api/projects is too).
package configsync

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/parser"
	"github.com/gocdnext/gocdnext/server/internal/scm"
	"github.com/gocdnext/gocdnext/server/internal/scm/bitbucket"
	gh "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/scm/gitlab"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// ErrFolderNotFound re-exports the scm-layer sentinel at this
// layer so callers don't need to import the scm package just to
// distinguish "repo reachable but folder absent" from a hard
// transport/auth error. Call sites match via errors.Is.
var ErrFolderNotFound = scm.ErrFolderNotFound

// Fetcher resolves the pipeline config folder for a known
// scm_source at a given revision. Implementations wrap one
// provider's contents API (GitHub, GitLab, Bitbucket Cloud).
// Tests supply an in-memory impl so the sync path can exercise
// end-to-end without a network call.
//
// configPath is the repo-relative folder (e.g. ".gocdnext",
// ".woodpecker", "apps/api/.gocdnext"). Empty → ".gocdnext" for
// backwards-compat.
//
// HeadSHA resolves a branch name to its current commit SHA. The
// trigger-seed path calls it when a pipeline has no modification
// yet (never received a push webhook): we fetch HEAD of the
// default branch, insert a modification row, and run against it
// so "Run latest" works on freshly-bound projects.
type Fetcher interface {
	Fetch(ctx context.Context, source store.SCMSource, ref, configPath string) ([]scm.RawFile, error)
	HeadSHA(ctx context.Context, source store.SCMSource, branch string) (string, error)
}

// CredentialResolver returns the auth token + API base a fetcher
// should use for a given (provider, repo URL) pair. Wired to the
// store so per-host org-level credentials (set in
// /settings/integrations) fill in when a per-project auth_ref is
// missing. Implementations must tolerate missing context or
// cipher state and return empty strings rather than erroring —
// the fetcher falls through to unauthenticated requests, which
// either work (public repos) or 401 naturally.
type CredentialResolver interface {
	ResolveAuthRef(ctx context.Context, provider, repoURL, scmAuthRef string) (authRef, apiBase string)
}

// MultiFetcher routes by source.Provider to the matching provider
// client. Clients are constructed on demand with a shared
// http.Client so connection reuse works across provider switches
// inside a single server process.
//
// APIBase overrides are per-provider so an operator with
// self-hosted GitLab CE + GitHub.com + Bitbucket Cloud can point
// each at the right endpoint. A host-scoped APIBase from the
// Resolver wins over the per-instance default.
type MultiFetcher struct {
	Client           *http.Client
	GitHubAPIBase    string // empty → gh.DefaultAPIBase
	GitLabAPIBase    string // empty → gitlab.DefaultAPIBase
	BitbucketAPIBase string // empty → bitbucket.DefaultAPIBase
	// Resolver, when set, gets a chance to fill in auth_ref +
	// api_base from org-level scm_credentials before the fetcher
	// hits the provider. nil disables the lookup and the
	// per-project source.AuthRef + per-instance APIBase are
	// used verbatim.
	Resolver CredentialResolver
}

func (f *MultiFetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// resolve applies the CredentialResolver (when configured) to
// fill in auth_ref + per-host api_base. Per-project
// source.AuthRef always wins; org-level credential is a pure
// fallback. Returns (authRef, apiBase); apiBase is empty when
// no host-scoped override is found — caller keeps its default.
func (f *MultiFetcher) resolve(
	ctx context.Context, source store.SCMSource, defaultAPIBase string,
) (authRef, apiBase string) {
	authRef = source.AuthRef
	apiBase = defaultAPIBase
	if f.Resolver != nil {
		resolvedAuth, resolvedBase := f.Resolver.ResolveAuthRef(
			ctx, source.Provider, source.URL, source.AuthRef,
		)
		if resolvedAuth != "" {
			authRef = resolvedAuth
		}
		if resolvedBase != "" {
			apiBase = resolvedBase
		}
	}
	return
}

// Fetch dispatches by provider. Unknown providers get a typed
// error so misconfiguration surfaces loud instead of silently
// returning empty.
func (f *MultiFetcher) Fetch(
	ctx context.Context, source store.SCMSource, ref, configPath string,
) ([]scm.RawFile, error) {
	switch source.Provider {
	case "github":
		owner, repo, err := gh.ParseRepoURL(source.URL)
		if err != nil {
			return nil, fmt.Errorf("configsync: parse github url: %w", err)
		}
		authRef, apiBase := f.resolve(ctx, source, f.GitHubAPIBase)
		return gh.FetchGocdnextFolder(ctx, f.client(), gh.Config{
			APIBase: apiBase,
			Owner:   owner,
			Repo:    repo,
			Token:   authRef,
		}, ref, configPath)
	case "gitlab":
		path, err := gitlab.ParseRepoURL(source.URL)
		if err != nil {
			return nil, fmt.Errorf("configsync: parse gitlab url: %w", err)
		}
		authRef, apiBase := f.resolve(ctx, source, f.GitLabAPIBase)
		return gitlab.FetchGocdnextFolder(ctx, f.client(), gitlab.Config{
			APIBase:     apiBase,
			ProjectPath: path,
			Token:       authRef,
		}, ref, configPath)
	case "bitbucket":
		ws, repo, err := bitbucket.ParseRepoURL(source.URL)
		if err != nil {
			return nil, fmt.Errorf("configsync: parse bitbucket url: %w", err)
		}
		authRef, apiBase := f.resolve(ctx, source, f.BitbucketAPIBase)
		return bitbucket.FetchGocdnextFolder(ctx, f.client(), bitbucket.Config{
			APIBase:   apiBase,
			Workspace: ws,
			RepoSlug:  repo,
			// Bitbucket convention: store the token as a raw OAuth /
			// "access token" string in auth_ref. Basic (App Password)
			// flow needs username + password, which means a richer
			// auth_ref shape — punt until the UI grows it.
			Token: authRef,
		}, ref, configPath)
	default:
		return nil, fmt.Errorf("configsync: provider %q not supported", source.Provider)
	}
}

// HeadSHA dispatches by provider like Fetch. Returns the commit
// SHA at the tip of `branch`.
func (f *MultiFetcher) HeadSHA(
	ctx context.Context, source store.SCMSource, branch string,
) (string, error) {
	switch source.Provider {
	case "github":
		owner, repo, err := gh.ParseRepoURL(source.URL)
		if err != nil {
			return "", fmt.Errorf("configsync: parse github url: %w", err)
		}
		authRef, apiBase := f.resolve(ctx, source, f.GitHubAPIBase)
		return gh.GetBranchHead(ctx, f.client(), gh.Config{
			APIBase: apiBase,
			Owner:   owner,
			Repo:    repo,
			Token:   authRef,
		}, branch)
	case "gitlab":
		path, err := gitlab.ParseRepoURL(source.URL)
		if err != nil {
			return "", fmt.Errorf("configsync: parse gitlab url: %w", err)
		}
		authRef, apiBase := f.resolve(ctx, source, f.GitLabAPIBase)
		return gitlab.GetBranchHead(ctx, f.client(), gitlab.Config{
			APIBase:     apiBase,
			ProjectPath: path,
			Token:       authRef,
		}, branch)
	case "bitbucket":
		ws, repo, err := bitbucket.ParseRepoURL(source.URL)
		if err != nil {
			return "", fmt.Errorf("configsync: parse bitbucket url: %w", err)
		}
		authRef, apiBase := f.resolve(ctx, source, f.BitbucketAPIBase)
		return bitbucket.GetBranchHead(ctx, f.client(), bitbucket.Config{
			APIBase:   apiBase,
			Workspace: ws,
			RepoSlug:  repo,
			Token:     authRef,
		}, branch)
	default:
		return "", fmt.Errorf("configsync: provider %q not supported", source.Provider)
	}
}

// GitHubFetcher is the GitHub-only Fetcher kept for tests and
// call sites that explicitly want to pin to GitHub. New code
// should use MultiFetcher so provider switching is free. The
// interface it implements is identical to MultiFetcher so a
// caller can swap one for the other without changes.
type GitHubFetcher struct {
	Client  *http.Client
	APIBase string
}

func (f *GitHubFetcher) Fetch(
	ctx context.Context, source store.SCMSource, ref, configPath string,
) ([]scm.RawFile, error) {
	m := &MultiFetcher{Client: f.Client, GitHubAPIBase: f.APIBase}
	return m.Fetch(ctx, source, ref, configPath)
}

func (f *GitHubFetcher) HeadSHA(
	ctx context.Context, source store.SCMSource, branch string,
) (string, error) {
	m := &MultiFetcher{Client: f.Client, GitHubAPIBase: f.APIBase}
	return m.HeadSHA(ctx, source, branch)
}

// ParseFiles turns the raw contents-API payload into domain
// pipelines, catching duplicate pipeline names across files so
// the caller can surface a validation error instead of silently
// overwriting one with the other at apply time.
//
// Empty f yields an empty slice (not an error) — the caller
// decides whether that's valid (bind with no pipelines yet).
func ParseFiles(files []scm.RawFile) ([]*domain.Pipeline, error) {
	seen := map[string]string{}
	out := make([]*domain.Pipeline, 0, len(files))
	for _, f := range files {
		if f.Name == "" {
			return nil, fmt.Errorf("configsync: config entry missing name")
		}
		fallback := strings.TrimSuffix(f.Name, filepath.Ext(f.Name))
		p, err := parser.ParseNamed(strings.NewReader(f.Content), "", fallback)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		if prev, dup := seen[p.Name]; dup {
			return nil, fmt.Errorf("pipeline %q defined twice: %s and %s", p.Name, prev, f.Name)
		}
		seen[p.Name] = f.Name
		out = append(out, p)
	}
	return out, nil
}
