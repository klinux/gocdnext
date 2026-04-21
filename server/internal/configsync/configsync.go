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
	gh "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// ErrFolderNotFound mirrors github.ErrFolderNotFound at this layer
// so callers don't have to import the github package just to
// distinguish "repo reachable but folder absent" from a hard
// transport/auth error. Call sites match via errors.Is.
var ErrFolderNotFound = gh.ErrFolderNotFound

// Fetcher resolves the pipeline config folder for a known scm_source
// at a given revision. Implementations wrap a provider-specific
// contents API (GitHub today; GitLab/Bitbucket later). Tests supply
// an in-memory impl so the sync path can exercise end-to-end
// without a network call.
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
	Fetch(ctx context.Context, scm store.SCMSource, ref, configPath string) ([]gh.RawFile, error)
	HeadSHA(ctx context.Context, scm store.SCMSource, branch string) (string, error)
}

// GitHubFetcher is the default Fetcher for github-hosted repos.
// Parses owner/repo out of scm.URL, passes scm.AuthRef as the
// bearer token when set. Returns an error when the scm.Provider
// isn't "github" — other providers add their own Fetcher impl.
type GitHubFetcher struct {
	Client  *http.Client
	APIBase string // empty -> github.DefaultAPIBase
}

func (f *GitHubFetcher) Fetch(ctx context.Context, scm store.SCMSource, ref, configPath string) ([]gh.RawFile, error) {
	cfg, err := f.configFor(scm)
	if err != nil {
		return nil, err
	}
	return gh.FetchGocdnextFolder(ctx, f.client(), cfg, ref, configPath)
}

func (f *GitHubFetcher) HeadSHA(ctx context.Context, scm store.SCMSource, branch string) (string, error) {
	cfg, err := f.configFor(scm)
	if err != nil {
		return "", err
	}
	return gh.GetBranchHead(ctx, f.client(), cfg, branch)
}

func (f *GitHubFetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (f *GitHubFetcher) configFor(scm store.SCMSource) (gh.Config, error) {
	if scm.Provider != "github" {
		return gh.Config{}, fmt.Errorf("configsync: provider %q not supported by GitHubFetcher", scm.Provider)
	}
	owner, repo, err := gh.ParseRepoURL(scm.URL)
	if err != nil {
		return gh.Config{}, fmt.Errorf("configsync: parse repo url: %w", err)
	}
	return gh.Config{
		APIBase: f.APIBase,
		Owner:   owner,
		Repo:    repo,
		Token:   scm.AuthRef,
	}, nil
}

// ParseFiles turns the raw contents-API payload into domain
// pipelines, catching duplicate pipeline names across files so
// the caller can surface a validation error instead of silently
// overwriting one with the other at apply time.
//
// Empty f yields an empty slice (not an error) — the caller
// decides whether that's valid (bind with no pipelines yet).
func ParseFiles(files []gh.RawFile) ([]*domain.Pipeline, error) {
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
