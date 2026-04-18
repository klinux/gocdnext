package webhook

import (
	"context"
	"errors"
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

// ConfigFetcher resolves the `.gocdnext/` folder for a known scm_source at a
// given revision. Implementations wrap a provider-specific contents API
// (GitHub today; GitLab/Bitbucket later). Tests supply an in-memory impl so
// the drift path can exercise end-to-end without a network call.
type ConfigFetcher interface {
	Fetch(ctx context.Context, scm store.SCMSource, ref string) ([]gh.RawFile, error)
}

// GitHubConfigFetcher is the default implementation. Parses owner/repo out of
// scm.URL, passes scm.AuthRef as the bearer token when set. Returns an error
// when the scm.Provider isn't "github" — other providers add their own
// ConfigFetcher impl.
type GitHubConfigFetcher struct {
	Client  *http.Client
	APIBase string // empty -> github.DefaultAPIBase
}

func (f *GitHubConfigFetcher) Fetch(ctx context.Context, scm store.SCMSource, ref string) ([]gh.RawFile, error) {
	if scm.Provider != "github" {
		return nil, fmt.Errorf("drift: provider %q not supported by GitHubConfigFetcher", scm.Provider)
	}
	owner, repo, err := gh.ParseRepoURL(scm.URL)
	if err != nil {
		return nil, fmt.Errorf("drift: parse repo url: %w", err)
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return gh.FetchGocdnextFolder(ctx, client, gh.Config{
		APIBase: f.APIBase,
		Owner:   owner,
		Repo:    repo,
		Token:   scm.AuthRef,
	}, ref)
}

// DriftOutcome reports what happened when a push arrived for a registered
// scm_source — the webhook handler surfaces this in its response body for
// observability.
type DriftOutcome struct {
	Attempted bool
	Applied   bool
	Error     string
	Revision  string
}

// applyDrift re-fetches the `.gocdnext/` folder at the push's revision and
// calls store.ApplyProject with its contents. The function is NOT
// transactional across fetch+apply (network + DB), so partial failures are
// reported via DriftOutcome.Error and the caller continues with the existing
// material-matching path against whatever state the DB currently has.
func (h *Handler) applyDrift(ctx context.Context, scm store.SCMSource, branch, revision string) DriftOutcome {
	out := DriftOutcome{Revision: revision}
	if h.fetcher == nil {
		return out
	}
	if branch != scm.DefaultBranch {
		// A push on a non-default branch doesn't drive config sync — the live
		// config tracks main only. We could broaden later (per-env configs).
		return out
	}
	out.Attempted = true

	files, err := h.fetcher.Fetch(ctx, scm, revision)
	if err != nil {
		out.Error = err.Error()
		return out
	}

	pipelines, err := parseConfigFiles(files)
	if err != nil {
		out.Error = fmt.Sprintf("parse: %v", err)
		return out
	}

	project, err := h.store.GetProjectByID(ctx, scm.ProjectID)
	if err != nil {
		out.Error = fmt.Sprintf("project lookup: %v", err)
		return out
	}

	// Feed the scm_source back through ApplyProject so its row stays
	// consistent with the binding the caller already established.
	scmInput := &store.SCMSourceInput{
		Provider:      scm.Provider,
		URL:           scm.URL,
		DefaultBranch: scm.DefaultBranch,
		WebhookSecret: scm.WebhookSecret,
		AuthRef:       scm.AuthRef,
	}

	if _, err := h.store.ApplyProject(ctx, store.ApplyProjectInput{
		Slug:        project.Slug,
		Name:        project.Name,
		Description: project.Description,
		Pipelines:   pipelines,
		SCMSource:   scmInput,
	}); err != nil {
		out.Error = fmt.Sprintf("apply: %v", err)
		return out
	}

	if err := h.store.MarkSCMSourceSynced(ctx, scm.ID, revision); err != nil {
		// Non-fatal — the state was applied, just the bookkeeping failed.
		h.log.Warn("drift: mark synced failed", "scm_source_id", scm.ID, "err", err)
	}

	out.Applied = true
	return out
}

func parseConfigFiles(files []gh.RawFile) ([]*domain.Pipeline, error) {
	seen := map[string]string{}
	out := make([]*domain.Pipeline, 0, len(files))
	for _, f := range files {
		if f.Name == "" {
			return nil, fmt.Errorf("config entry missing name")
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

// driftLookup wraps the common "find scm_source for this push" call — the
// handler only fires applyDrift when a match exists. Swallows the
// not-found sentinel into (nil, false) so the caller doesn't have to import
// errors just for the sentinel comparison.
func (h *Handler) driftLookup(ctx context.Context, cloneURL string) (store.SCMSource, bool) {
	scm, err := h.store.FindSCMSourceByURL(ctx, cloneURL)
	if err != nil {
		if !errors.Is(err, store.ErrSCMSourceNotFound) {
			h.log.Warn("drift: scm_source lookup failed", "url", cloneURL, "err", err)
		}
		return store.SCMSource{}, false
	}
	return scm, true
}
