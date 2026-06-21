package compliance

import (
	"strings"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// PipelineName is the reserved name of the server-owned synthetic compliance
// pipeline. It is created for a governed project that ships no pipeline of its
// own, so mandatory policy jobs still run on every default-branch push. Repo
// YAML may not name a pipeline with this prefix (enforced at apply time).
const PipelineName = "_compliance"

// IsReservedPipelineName reports whether a pipeline name is reserved for
// compliance (the synthetic pipeline). Repo pipelines are rejected if they use
// it, so a developer can't pre-create / impersonate the enforced pipeline.
func IsReservedPipelineName(name string) bool {
	return strings.HasPrefix(name, PipelineName)
}

// ComplianceMaterial builds the NON-SUPPRESSIBLE git material for a governed
// pipeline: it fires on push to the default branch with no path filter, so the
// repo's `when.event/branch/paths` can't stop a compliance pipeline from
// running. The fingerprint (url+branch) matches the webhook's, and an existing
// repo material on the same (url, default-branch) is overwritten by the upsert —
// stripping any path/event narrowing the repo declared.
func ComplianceMaterial(scmURL, defaultBranch string) domain.Material {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	return domain.Material{
		Type:        domain.MaterialGit,
		Fingerprint: domain.GitFingerprint(scmURL, defaultBranch),
		AutoUpdate:  true,
		Implicit:    true,
		Git: &domain.GitMaterial{
			URL:                 domain.HTTPCloneURL(scmURL),
			Branch:              defaultBranch,
			Events:              []string{"push"},
			AutoRegisterWebhook: true,
			// No Paths: a compliance trigger is never path-filtered.
		},
	}
}

// SyntheticPipeline returns the bare synthetic compliance pipeline: no stages or
// jobs of its own (the policy merge fills those in as the effective definition)
// plus the non-suppressible material. Used only when a governed project has no
// pipeline of its own to merge into.
func SyntheticPipeline(scmURL, defaultBranch string) domain.Pipeline {
	return domain.Pipeline{
		Name:      PipelineName,
		Materials: []domain.Material{ComplianceMaterial(scmURL, defaultBranch)},
	}
}
