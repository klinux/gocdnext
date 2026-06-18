package webhook

import (
	"context"
	"errors"
	"fmt"

	"github.com/gocdnext/gocdnext/server/internal/configsync"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// ConfigFetcher is an alias for configsync.Fetcher kept here so
// existing webhook call sites (and tests) don't churn. The actual
// type lives in configsync — both the webhook push-drift path
// and the project-apply initial-sync path need it, and neither
// should import the other.
type ConfigFetcher = configsync.Fetcher

// GitHubConfigFetcher aliases configsync.GitHubFetcher for the
// same reason: a back-compat name for callers that wire the
// default implementation into webhook.Handler.
type GitHubConfigFetcher = configsync.GitHubFetcher

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
		// Server started without a ConfigFetcher wired — drift is
		// silently impossible. Log it once per push so an operator
		// staring at "why didn't my config update?" finds the
		// answer in the server logs instead of in the code.
		h.log.Info("github webhook: drift skipped — no config fetcher wired",
			"scm_source_id", scm.ID, "project_id", scm.ProjectID,
			"branch", branch, "revision", revision)
		return out
	}
	if branch != scm.DefaultBranch {
		// A push on a non-default branch doesn't drive config sync — the live
		// config tracks main only. Broadening this guard is intentionally
		// gated on a separate follow-up: every push currently overwrites
		// the project's global definition, so feature branches with
		// destructive YAML changes (removed approval gates, swapped
		// profiles, etc) would otherwise land project-wide. The right
		// shape is "only re-apply when the pushed branch is itself a
		// registered material" — separate commit.
		//
		// Log the skip — pre-v0.14.3 this was silent and operators
		// staring at "I pushed to my project's default branch and
		// drift didn't fire" had no signal whether the branch
		// comparison or something later in the path was at fault.
		// Includes both the pushed branch AND the configured default
		// so a typo in either is obvious side-by-side.
		h.log.Info("github webhook: drift skipped — branch is not the project's default",
			"scm_source_id", scm.ID, "project_id", scm.ProjectID,
			"pushed_branch", branch, "default_branch", scm.DefaultBranch,
			"revision", revision)
		return out
	}
	out.Attempted = true

	// Project first — we need its config_path to know which
	// folder to fetch. Failing this before the network call
	// saves one GitHub round-trip when the row is missing.
	project, err := h.store.GetProjectByID(ctx, scm.ProjectID)
	if err != nil {
		out.Error = fmt.Sprintf("project lookup: %v", err)
		return out
	}

	files, err := h.fetcher.Fetch(ctx, scm, revision, project.ConfigPath)
	if err != nil {
		out.Error = err.Error()
		return out
	}

	pipelines, err := configsync.ParseFiles(files)
	if err != nil {
		out.Error = fmt.Sprintf("parse: %v", err)
		return out
	}

	// Feed the scm_source back through ApplyProject so its row
	// stays consistent with the binding the caller already
	// established. Leaving WebhookSecret empty signals "preserve
	// existing ciphertext" — drift re-apply is not the path
	// where we rotate credentials.
	scmInput := &store.SCMSourceInput{
		Provider:      scm.Provider,
		URL:           scm.URL,
		DefaultBranch: scm.DefaultBranch,
		AuthRef:       scm.AuthRef,
	}

	// Apply the same implicit-material synthesis the UI's apply +
	// sync paths do. Without this, a config-only push that drives
	// drift would rebuild the project's pipeline rows WITHOUT the
	// implicit "this project's repo" material — subsequent pushes
	// would then fail the fingerprint lookup and silently 202 with
	// no run. (Same code path the project-apply and project-sync
	// handlers already go through.)
	configsync.InjectImplicitProjectMaterial(pipelines, scmInput)

	// Resolve runner profiles before ApplyProject — same step the
	// CLI apply handler runs. Without this, the persisted
	// pipelines.definition carries `Resources: zeroed` even when
	// the YAML's `agent.profile: foo` would have filled them from
	// the profile's defaults.
	if err := h.store.ResolveProfiles(ctx, pipelines); err != nil {
		out.Error = fmt.Sprintf("resolve profiles: %v", err)
		return out
	}
	if err := h.store.ResolveClusters(ctx, pipelines); err != nil {
		out.Error = fmt.Sprintf("resolve clusters: %v", err)
		return out
	}

	if _, err := h.store.ApplyProject(ctx, store.ApplyProjectInput{
		Slug:        project.Slug,
		Name:        project.Name,
		Description: project.Description,
		ConfigPath:  project.ConfigPath,
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
