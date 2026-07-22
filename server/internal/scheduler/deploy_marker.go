package scheduler

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// ErrDeployRevisionNotCorrelatable is a terminal config error for a NATIVE deploy: the
// correlation anchor could not be resolved to a full git SHA. Native needs the FULL SHA
// ArgoCD reports to confirm the RIGHT commit deployed — an EMPTY anchor disables the
// revision check entirely (Evaluate accepts any Synced+Healthy state), and a non-SHA
// anchor pins the watch to something ArgoCD can never report, which stalls until the
// deadline instead of failing clearly. A retry fails identically, so the dispatcher
// terminalises the job loud (#39-style).
//
// Deliberately anchor-centric, not "deploy.version"-centric: the anchor may come from
// deploy.revision, from a SHA-shaped deploy.version, or from the run's own commit, and
// telling a user to "leave version empty" is actively wrong when they already did and
// the run simply has no usable commit.
var ErrDeployRevisionNotCorrelatable = errors.New("native deploy correlation revision must resolve to a full git SHA; set deploy.revision explicitly when the run commit is unavailable or is not the Application's source revision")

var (
	fullSHARE = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	hexShaRE  = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)
)

// correlationRevision resolves the FULL git SHA the native watch correlates against
// (ArgoCD reports the full SHA in .status.sync.revision / syncResult.revision), from the
// resolved display version, the resolved deploy.revision, and the run's own commit
// (CI_COMMIT_SHA). Ladder, first match wins:
//
//  1. deploy.revision set                 → that SHA (the explicit anchor)
//  2. else, SHA-shaped deploy.version     → that SHA (a deliberate commit pin)
//  3. else (non-SHA or defaulted version) → the run's commit
//
// POST-CONDITION on every branch: the result is a full 40-hex SHA or a terminal error.
// It is stated as a property of the OUTPUT rather than a check per branch so a future
// path cannot quietly skip it — which is exactly how the old `explicit` short-circuit
// returned the run commit unvalidated. There is deliberately no such bypass now: an
// omitted version simply resolves to the short sha, which rule 2 or 3 then validates
// like any other.
//
// Version stays the display/ledger string untouched; only the correlation anchor is
// resolved here, and deploy.revision never substitutes for it.
func correlationRevision(jobName, version, revision, runCommit string) (string, error) {
	// The run's commit is usable as an anchor — and as the expansion base for a short
	// hex — ONLY if it is itself a full SHA. buildCIVars takes it from primaryRevision
	// without validating the shape, so a non-git material (or an upstream-only run) can
	// put a tag or a UUID here; anchoring on that would stall until the deadline.
	run := strings.ToLower(strings.TrimSpace(runCommit))
	if !fullSHARE.MatchString(run) {
		run = ""
	}

	if rev := strings.ToLower(strings.TrimSpace(revision)); rev != "" {
		return resolveAnchor(jobName, "deploy.revision", rev, run)
	}
	if v := strings.ToLower(strings.TrimSpace(version)); hexShaRE.MatchString(v) {
		return resolveAnchor(jobName, "deploy.version", v, run)
	}
	if run == "" {
		return "", fmt.Errorf(
			"%w: job %s has no usable run commit to correlate against (deploy.version is not a SHA)",
			ErrDeployRevisionNotCorrelatable, jobName)
	}
	return run, nil
}

// resolveAnchor expands a hex candidate to the full SHA the watch can match: a full
// 40-hex is taken as-is (a pin, valid even when the run has no commit), a short hex only
// when it prefixes the run's commit — we cannot expand a foreign short SHA without git
// access, and guessing would be worse than failing.
func resolveAnchor(jobName, field, candidate, run string) (string, error) {
	switch {
	case fullSHARE.MatchString(candidate):
		return candidate, nil
	case hexShaRE.MatchString(candidate) && run != "" && strings.HasPrefix(run, candidate):
		return run, nil
	default:
		return "", fmt.Errorf("%w: job %s %s %q", ErrDeployRevisionNotCorrelatable, jobName, field, candidate)
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
