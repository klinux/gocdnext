package scheduler

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/gocdnext/gocdnext/server/pkg/refs"
)

// ErrDeployVersionEmpty is returned by BuildAssignment when a deploy
// job's version resolves to empty — the version was omitted AND the
// run has no commit short sha to default to (#39). Like
// ErrNeedsRefUnresolved it is a per-run CONFIG error the dispatcher
// terminalises (a retry would resolve to empty again forever).
// Note deploy.revision does NOT satisfy this: it is the correlation anchor, not the
// ledger label, so a run that can't produce a version still fails here even when the
// anchor is pinned. Labelling a deployment with a raw SHA behind the user's back would
// be worse than asking for one.
var ErrDeployVersionEmpty = errors.New("deploy.version omitted but CI_COMMIT_SHORT_SHA is unavailable for this run; set deploy.version explicitly")

// ErrDeployVersionUnresolved wraps ANY failure resolving a non-empty
// deploy.version — or deploy.revision, which goes through the same
// resolver (#39): a `${{ CI_* }}` the parser accepted by shape
// but this run doesn't carry (CI_TAG_NAME on a non-tag run,
// CI_COMMIT_SHORT_SHA with no git material), or a needs-ref shape that
// slipped past the parser's allow-list. All are per-run config errors
// — the dispatcher terminalises them so the job fails loud once
// instead of retrying the identical failure forever. The caller names
// which field failed, so the sentinel stays field-agnostic.
var ErrDeployVersionUnresolved = errors.New("a deploy marker reference could not be resolved for this run")

// resolveDeployVersion resolves a non-empty deploy.version through the
// same pipeline the env uses — the `${{ needs.*.outputs.* }}` pre-pass,
// the strict `${{ vars.NAME }}` + `${{ CI_* }}` pass, then the soft
// shell-style `${CI_*}` pass for parity with plugin settings. It
// resolves against CI vars + the pipeline `variables:` (via the
// `vars.` namespace, #281) ONLY — NEVER secrets: the version is
// persisted in deployment_revisions and shown in the Environments UI.
// The `vars.` namespace is safe because pkg/refs binds it strictly to
// `vars` (the non-secret `variables:` map) — a bare `${{ NAME }}` or a
// `${{ secrets.X }}` was already rejected at apply by the parser's
// allow-list; here we additionally can't leak because the Secrets
// namespace is left nil. ANY resolution failure is wrapped in
// ErrDeployVersionUnresolved so the dispatcher terminalises it rather
// than retrying an identical failure forever (#39).
func resolveDeployVersion(raw string, needs NeedsOutputs, matrix MatrixNeedsOutputs, dims MatrixDimNames, vars, ciVars map[string]string) (string, error) {
	v, err := substituteNeedsRefs(raw, needs, matrix, dims)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDeployVersionUnresolved, err)
	}
	// NS with Secrets deliberately nil: a `secrets.X` ref here can never
	// resolve (defence in depth on top of the parser's allow-list). vars.
	// → the pipeline variables; bare + CI_* → ciVars.
	v, err = substituteRefsNS(v, refs.Namespaces{Vars: vars}, ciVars)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDeployVersionUnresolved, err)
	}
	// Soft shell-style pass for `1.${CI_RUN_COUNTER}.…` parity with
	// plugin settings. For plugin settings an unresolved ${VAR} is left
	// literal (a runtime shell may expand it) — but deploy.version is
	// metadata persisted as-is, there is no later shell. A leftover
	// ${CI_*} means a CI var absent from THIS run (e.g. ${CI_TAG_NAME}
	// on a non-tag run); persisting it literally would be a lie. Catch
	// it and terminalise rather than recording a bogus version.
	v = substituteShellVars(v, ciVars)
	if leftoverCIShellRE.MatchString(v) {
		return "", fmt.Errorf("%w: unresolved ${CI_*} shell reference (CI var not present this run)", ErrDeployVersionUnresolved)
	}
	return v, nil
}

// leftoverCIShellRE detects a shell-style ${CI_...} that the soft pass
// could not resolve — only the CI_ namespace, since a non-CI ${VAR}
// in a version is the operator's literal choice, not a gocdnext ref.
// The body is `[^}]*` (not just word chars) so shell modifier forms
// substituteShellVars never touches — ${CI_TAG_NAME:-dev},
// ${CI_TAG_NAME?missing} — are caught too, rather than persisting a
// misleading literal in a field that has no later shell to expand it.
var leftoverCIShellRE = regexp.MustCompile(`\$\{CI_[^}]*\}`)
