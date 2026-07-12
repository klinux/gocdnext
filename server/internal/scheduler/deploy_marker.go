package scheduler

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// ErrDeployVersionNotCorrelatable is a terminal config error for a NATIVE deploy: the
// user registered an ArgoCD target and set deploy.version to something that isn't a
// git commit SHA (a semver/tag/branch, or a short hex we can't expand without git
// access). Native needs the FULL SHA ArgoCD reports to confirm the RIGHT commit
// deployed; an unpinned watch could accept stale Synced+Healthy state. A retry would
// fail identically, so the dispatcher terminalises the job loud (#39-style).
var ErrDeployVersionNotCorrelatable = errors.New("deploy.version is not a correlatable git commit SHA for a native deploy; use the full/short commit SHA, or leave version empty to use the run's commit")

var (
	fullSHARE = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	hexShaRE  = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)
)

// correlationRevision resolves the FULL git SHA the native watch correlates against
// (ArgoCD reports the full SHA in .status.sync.revision / syncResult.revision), given
// the resolved display version. `explicit` is whether the user set deploy.version;
// fullSHA is the run's own commit (CI_COMMIT_SHA). Rules (reviewer-pinned):
//   - default (not explicit)      → the run's commit (fullSHA)
//   - explicit full 40-hex SHA    → that SHA, lowercased (a deliberately pinned commit)
//   - explicit short hex that prefixes the run's commit → expand to fullSHA
//   - anything else               → ErrDeployVersionNotCorrelatable (terminal)
//
// Version stays the display/ledger string untouched; only the correlation anchor is
// resolved here.
func correlationRevision(jobName, display string, explicit bool, fullSHA string) (string, error) {
	if !explicit {
		return fullSHA, nil
	}
	v := strings.ToLower(strings.TrimSpace(display))
	full := strings.ToLower(strings.TrimSpace(fullSHA))
	switch {
	case fullSHARE.MatchString(v):
		return v, nil
	case hexShaRE.MatchString(v) && full != "" && strings.HasPrefix(full, v):
		return full, nil
	default:
		return "", fmt.Errorf("%w: job %s deploy.version %q", ErrDeployVersionNotCorrelatable, jobName, display)
	}
}

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
