package projects

import (
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// injectImplicitProjectMaterial adds a git material derived from the
// project's scm_source to every pipeline that doesn't already declare
// a git material pointing at the same URL. Lets operators write
// pipelines whose YAML omits the obvious "this project's repo"
// material — it's already implied by the scm_source binding.
//
// No-op when `scm` is nil or has no URL (legacy flows, manual/upstream
// only projects). Pipelines that explicitly declare a git material
// for a *different* URL (template repo, sibling service) keep their
// declaration untouched — the implicit one rides alongside. Pipelines
// whose explicit git url matches the scm's url short-circuit the
// injection so there's no duplicate row at apply time.
func injectImplicitProjectMaterial(pipelines []*domain.Pipeline, scm *store.SCMSourceInput) {
	if scm == nil || scm.URL == "" {
		return
	}
	branch := scm.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	normalizedScmURL := domain.NormalizeGitURL(scm.URL)

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
		p.Materials = append(p.Materials, domain.Material{
			Type:        domain.MaterialGit,
			Fingerprint: domain.GitFingerprint(scm.URL, branch),
			AutoUpdate:  true,
			Implicit:    true,
			Git: &domain.GitMaterial{
				URL:                 scm.URL,
				Branch:              branch,
				Events:              append([]string(nil), events...),
				AutoRegisterWebhook: true,
			},
		})
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
