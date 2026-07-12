package scheduler

import (
	"fmt"
	"sort"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// buildMatrixDims builds the MatrixDimNames table (issue #21) for the upstreams that
// have matrix rows — the substitution layer needs it to resolve the `matrix[apac]`
// 1-dim shortcut. Dimension order is lex-sorted for deterministic 1-dim behaviour.
// Extracted so BuildAssignment and the native deploy-marker path share one impl.
func buildMatrixDims(def domain.Pipeline, matrixNeedsOutputs MatrixNeedsOutputs) MatrixDimNames {
	dims := make(MatrixDimNames, len(matrixNeedsOutputs))
	for upstreamName := range matrixNeedsOutputs {
		upstream, ok := findJob(def.Jobs, upstreamName)
		if !ok || len(upstream.Matrix) == 0 {
			continue
		}
		d := make([]string, 0, len(upstream.Matrix))
		for k := range upstream.Matrix {
			d = append(d, k)
		}
		sort.Strings(d)
		dims[upstreamName] = d
	}
	return dims
}

// resolveDeployMarkerVersion resolves a deploy job's recorded version (#39): the
// explicit deploy.version with the SAME needs.outputs + CI-vars substitution the env
// uses (but NOT secrets — the version is persisted + surfaced in the UI), or the
// commit short sha by default. Empty → ErrDeployVersionEmpty (terminal). Shared by
// BuildAssignment (plugin path) and the native takeover so both resolve identically.
func resolveDeployMarkerVersion(jobName string, jobDef domain.Job, needs NeedsOutputs, matrix MatrixNeedsOutputs, dims MatrixDimNames, ciVars map[string]string) (string, error) {
	version := jobDef.Deploy.Version
	if version == "" {
		version = ciVars["CI_COMMIT_SHORT_SHA"]
	} else {
		v, err := resolveDeployVersion(version, needs, matrix, dims, ciVars)
		if err != nil {
			// Wrapped in ErrDeployVersionUnresolved — terminal.
			return "", fmt.Errorf("scheduler: job %s: %w", jobName, err)
		}
		version = v
	}
	if version == "" {
		return "", fmt.Errorf("%w: job %s deploy.environment %q",
			ErrDeployVersionEmpty, jobName, jobDef.Deploy.Environment)
	}
	return version, nil
}
