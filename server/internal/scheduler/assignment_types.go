package scheduler

import "strings"

type revisionSnapshot struct {
	Revision string `json:"revision"`
	Branch   string `json:"branch"`
}

// BuildAssignment composes a JobAssignment proto from the run's pipeline
// snapshot + the dispatchable job row + the pipeline's material rows +
// already-resolved secrets (keyed by name) + pre-signed artifact
// downloads. Secret values land in env alongside the pipeline's own
// variables AND are echoed into LogMasks so the runner can replace them
// with *** in every log line. `downloads` is the list of upstream-job
// artefacts this job declares via `needs_artifacts:`; nil/empty when
// the job has no deps.
//
// `profileEnv` is the merged env (plain + decrypted secrets) from the
// job's runner profile; it lays in FIRST so explicit pipeline / job /
// project-secret values can override profile defaults. `profileMasks`
// is the matching list of profile secret VALUES — same redaction
// contract as job secrets.
// DeployTarget is the resolved deploy marker for a dispatched job
// (#39): the environment name and the version string, both ready to
// record as a deployment_revision. nil when the job has no `deploy:`
// block. The version is resolved here (not at result time) because
// BuildAssignment already holds the needs.outputs + CI-var sources;
// it is deliberately NOT resolved against secrets — the version lands
// in deployment_revisions and the Environments UI, so it must stay
// non-sensitive.
type DeployTarget struct {
	Environment string
	Version     string
}

// decomposeMatrixKey splits a matrix_key ("ARCH=amd64,OS=linux",
// built by store.matrixKey: sorted dims, `,`-joined, `=`-paired) into
// its per-dimension map for env injection (#42). The parser guarantees
// names/values are clean; a malformed pair (only possible on a
// pre-#42 persisted definition) is skipped rather than panicking.
func decomposeMatrixKey(key string) map[string]string {
	if key == "" {
		return nil
	}
	parts := strings.Split(key, ",")
	out := make(map[string]string, len(parts))
	for _, pair := range parts {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			continue
		}
		out[k] = v
	}
	return out
}
