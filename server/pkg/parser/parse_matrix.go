package parser

import (
	"fmt"
	"strings"
)

func flattenMatrix(entries []map[string][]string) map[string][]string {
	out := map[string][]string{}
	for _, e := range entries {
		for k, vs := range e {
			out[k] = append(out[k], vs...)
		}
	}
	return out
}

// validateMatrixDimensions enforces two parse-time invariants on a
// flattened matrix (issue #21 review fix):
//
//   - every declared dimension MUST have at least one value. An
//     empty list (`shard: []`) used to silently get dropped by the
//     runtime matrixCombos walker, producing a row with no
//     matrix_key — which then fell into the bare-ref path and
//     bypassed the "matrix upstream needs a selector" contract.
//   - within a dimension, values MUST be unique. Duplicates
//     (`shard: [apac, apac]`) used to expand to multiple rows with
//     the same canonical matrix_key, which the scheduler's
//     groupNeedsOutputs then overwrote silently in the rowMap
//     lookup. The downstream's `${{ needs.X.matrix[apac].outputs.Y }}`
//     would resolve to whichever row was iterated last — operator-
//     invisible non-determinism. Reject loud at parse so the
//     pipeline doesn't apply.
//
// jobName is folded into the error so the operator finds the
// offending job directly from the parser message.
// validateMatrixDimNames guards that each matrix dimension can be
// safely decomposed into a per-job env var at dispatch (#42 — e.g.
// `OS: [linux]` exposes `$OS=linux`):
//
//   - the name is a valid env identifier (reuses outputEnvRE);
//   - it isn't a reserved CI_/GOCDNEXT_ prefix (those are built-ins,
//     and GOCDNEXT_MATRIX is the combined key the agent also sees);
//   - it doesn't collide with a declared pipeline/job variable or a
//     secret — `$DIM` would otherwise be ambiguous, so we reject the
//     name clash at apply time instead of picking a silent winner;
//   - no value carries `,` or `=`, the matrix_key separators (a value
//     with one would make the key un-parseable back into dims).
func validateMatrixDimNames(jobName string, matrix map[string][]string, pipelineVars, jobVars map[string]string, secrets []string, idTokens map[string]IDTokenDef) error {
	taken := make(map[string]string, len(pipelineVars)+len(jobVars)+len(secrets)+len(idTokens))
	for k := range pipelineVars {
		taken[k] = "a pipeline variable"
	}
	for k := range jobVars {
		taken[k] = "a job variable"
	}
	for _, s := range secrets {
		taken[s] = "a secret"
	}
	// id_tokens become env vars too — and are injected AFTER the matrix
	// decomposition at dispatch, so a token of the same name would
	// silently overwrite the dimension ($OS would be a JWT, not the
	// matrix value). Reject the clash at apply.
	for k := range idTokens {
		taken[k] = "an id_token"
	}
	for dim, values := range matrix {
		if !outputEnvRE.MatchString(dim) {
			return fmt.Errorf("job %q: matrix dimension %q is not a valid env var name — start with a letter or _, then letters/digits/_ (it becomes $%s at dispatch)", jobName, dim, dim)
		}
		if strings.HasPrefix(dim, "CI_") || strings.HasPrefix(dim, "GOCDNEXT_") {
			return fmt.Errorf("job %q: matrix dimension %q uses a reserved prefix (CI_/GOCDNEXT_) — those are gocdnext built-ins", jobName, dim)
		}
		if src, dup := taken[dim]; dup {
			return fmt.Errorf("job %q: matrix dimension %q collides with %s of the same name — a matrix dim becomes $%s, so the name must be unique", jobName, dim, src, dim)
		}
		for _, v := range values {
			if strings.ContainsAny(v, ",=") {
				return fmt.Errorf("job %q: matrix dimension %q value %q contains ',' or '=' — those separate the matrix key, so values can't use them", jobName, dim, v)
			}
		}
	}
	return nil
}

func validateMatrixDimensions(jobName string, matrix map[string][]string) error {
	for dim, values := range matrix {
		if len(values) == 0 {
			return fmt.Errorf("job %q: matrix dimension %q has no values; remove the dimension or supply at least one value", jobName, dim)
		}
		seen := make(map[string]struct{}, len(values))
		for _, v := range values {
			if _, dup := seen[v]; dup {
				return fmt.Errorf("job %q: matrix dimension %q has duplicate value %q; values within a dimension must be unique", jobName, dim, v)
			}
			seen[v] = struct{}{}
		}
	}
	return nil
}
