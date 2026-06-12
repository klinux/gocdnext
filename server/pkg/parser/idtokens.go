package parser

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// envNamePattern is the POSIX env-var charset. Compiled once —
// parse runs on every apply/sync, no per-call regexp churn.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// idTokensCap bounds the per-job token count. Real pipelines need
// 1-3 (GCP + Vault + maybe AWS); past the cap the operator is
// almost certainly generating YAML wrong, and each entry costs an
// RSA signature at dispatch.
const idTokensCap = 16

// validateIDTokensDeclaration checks the id_tokens block of one job
// and converts it to the domain shape. Everything here fails at
// APPLY time with the job name in the message — a malformed token
// request must never reach dispatch where the failure would surface
// as a cryptic per-run error.
//
// Rules:
//   - env var name: POSIX charset, no CI_/GOCDNEXT_ prefix (those
//     namespaces belong to the platform; letting a token shadow
//     CI_COMMIT_SHA invites spoofing-shaped bugs);
//   - no collision with the job's variables:/secrets: NOR the
//     pipeline-level variables: — all three land in the same env
//     map at dispatch (pipeline vars first, then job vars, then
//     the token), so any collision would resolve by silent
//     map-layering order in BuildAssignment;
//   - aud: required, non-blank, every entry non-blank.
func validateIDTokensDeclaration(jobName string, jd JobDef, pipelineVars map[string]string) (map[string]domain.IDTokenSpec, error) {
	if len(jd.IDTokens) > idTokensCap {
		return nil, fmt.Errorf(
			"job %q: id_tokens has %d entries — cap is %d",
			jobName, len(jd.IDTokens), idTokensCap)
	}
	secretNames := make(map[string]struct{}, len(jd.Secrets))
	for _, s := range jd.Secrets {
		secretNames[s] = struct{}{}
	}

	specs := make(map[string]domain.IDTokenSpec, len(jd.IDTokens))
	for envName, def := range jd.IDTokens {
		if !envNamePattern.MatchString(envName) {
			return nil, fmt.Errorf(
				"job %q: id_tokens key %q is not a valid env var name (want [A-Za-z_][A-Za-z0-9_]*)",
				jobName, envName)
		}
		upper := strings.ToUpper(envName)
		if strings.HasPrefix(upper, "CI_") || strings.HasPrefix(upper, "GOCDNEXT_") {
			return nil, fmt.Errorf(
				"job %q: id_tokens key %q uses a reserved prefix (CI_/GOCDNEXT_ belong to the platform)",
				jobName, envName)
		}
		if _, clash := jd.Variables[envName]; clash {
			return nil, fmt.Errorf(
				"job %q: id_tokens key %q collides with a `variables:` entry of the same name",
				jobName, envName)
		}
		if _, clash := pipelineVars[envName]; clash {
			return nil, fmt.Errorf(
				"job %q: id_tokens key %q collides with a pipeline-level `variables:` entry of the same name",
				jobName, envName)
		}
		if _, clash := secretNames[envName]; clash {
			return nil, fmt.Errorf(
				"job %q: id_tokens key %q collides with a `secrets:` entry of the same name",
				jobName, envName)
		}
		if len(def.Aud) == 0 {
			return nil, fmt.Errorf(
				"job %q: id_tokens entry %q is missing `aud` — the audience is required (no default; it must match the cloud provider's expected audience exactly)",
				jobName, envName)
		}
		auds := make([]string, 0, len(def.Aud))
		for _, a := range def.Aud {
			a = strings.TrimSpace(a)
			if a == "" {
				return nil, fmt.Errorf(
					"job %q: id_tokens entry %q has a blank `aud` value",
					jobName, envName)
			}
			auds = append(auds, a)
		}
		specs[envName] = domain.IDTokenSpec{Aud: auds}
	}
	return specs, nil
}
