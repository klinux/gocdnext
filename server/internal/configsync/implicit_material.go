package configsync

import (
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// InjectImplicitProjectMaterial adds a git material derived from the
// project's scm_source to every pipeline that doesn't already declare
// a git material pointing at the same URL. Lets operators write
// pipelines whose YAML omits the obvious "this project's repo"
// material — it's already implied by the scm_source binding.
//
// Lives in configsync (not api/projects) so the three call sites that
// need it can share one implementation without crossing the
// webhook ↔ api/projects layering rule. Today's call sites: the
// project-apply handler, the project-sync handler, and the webhook
// drift path. Adding a fourth call site that needs the same shape
// (e.g. a future CLI apply) imports configsync just like the others.
//
// No-op when `scm` is nil or has no URL (legacy flows, manual/upstream
// only projects). Pipelines that explicitly declare a git material
// for a *different* URL (template repo, sibling service) keep their
// declaration untouched — the implicit one rides alongside. Pipelines
// whose explicit git url matches the scm's url short-circuit the
// injection so there's no duplicate row at apply time.
//
// The material's `Git.URL` is stored as a clonable HTTPS URL via
// HTTPCloneURL so the agent's `git clone` always sees a fully-
// qualified URL even when the scm_source row was canonicalised to
// the scheme-less form at store time.
func InjectImplicitProjectMaterial(pipelines []*domain.Pipeline, scm *store.SCMSourceInput) {
	if scm == nil || scm.URL == "" {
		return
	}
	defaultBranch := scm.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	normalizedScmURL := domain.NormalizeGitURL(scm.URL)
	cloneURL := domain.HTTPCloneURL(scm.URL)

	for _, p := range pipelines {
		if p == nil {
			continue
		}
		if pipelineHasGitForURL(p, normalizedScmURL) {
			continue
		}
		events := p.TriggerEvents
		if len(events) == 0 {
			// Conservative default: push only. PR opt-in is
			// explicit via top-level `when.event: [pull_request]`
			// so operators don't accidentally double-fire runs
			// on every PR sync for a repo they just bound.
			events = []string{"push"}
		}
		// `when.branch:` whitelists which branches fire this pipeline.
		// Empty → fall back to the scm_source default branch (today's
		// single-branch behaviour). Non-empty → emit ONE implicit
		// material per branch so each push fingerprint (URL+branch)
		// matches a distinct row. This is how a single pipeline
		// declaration can serve multiple branches without forcing the
		// operator to copy/paste explicit material blocks.
		branches := p.TriggerBranches
		if len(branches) == 0 {
			branches = []string{defaultBranch}
		}
		for _, branch := range branches {
			p.Materials = append(p.Materials, domain.Material{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint(scm.URL, branch),
				AutoUpdate:  true,
				Implicit:    true,
				Git: &domain.GitMaterial{
					URL:                 cloneURL,
					Branch:              branch,
					Events:              append([]string(nil), events...),
					AutoRegisterWebhook: true,
					// when.paths lowers onto the implicit material the
					// same way when.event does — the webhook filters
					// materials by Paths against the delivery's
					// changed-file set.
					Paths: append([]string(nil), p.TriggerPaths...),
				},
			})
		}
	}
}

func pipelineHasGitForURL(p *domain.Pipeline, normalizedURL string) bool {
	for _, m := range p.Materials {
		if m.Type != domain.MaterialGit || m.Git == nil {
			continue
		}
		if domain.NormalizeGitURL(m.Git.URL) == normalizedURL {
			return true
		}
	}
	return false
}
