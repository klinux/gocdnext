package scheduler

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/gocdnext/gocdnext/server/pkg/refs"
)

// The generic `${{ NAME }}` / `${VAR}` substitution core lives in
// pkg/refs (#44) so the scheduler and the CLI's run-local simulator
// resolve refs identically. These aliases keep the short names the
// call sites and the needs-ref pass below already use.
var (
	refPattern             = refs.RefPattern
	substituteRefs         = refs.SubstituteRefs
	substituteShellVars    = refs.SubstituteShellVars
	substituteRefsMap      = refs.SubstituteRefsMap
	substituteShellVarsMap = refs.SubstituteShellVarsMap
	dedupeSorted           = refs.DedupeSorted
)

// needsRefPattern matches the dotted `needs.<job>.outputs.<alias>`
// form INSIDE a `${{ ... }}` wrapper. Each segment is validated
// separately so error messages can point at the bad piece:
//
//	${{ needs.bump.outputs.next }}                  ← parses
//	${{ needs.image-copy.outputs.promoted-digest }} ← parses (kebab job + alias)
//	${{ needs.bump.outputs }}                       ← rejected — missing alias
//	${{ needs.bump.foo.next }}                      ← rejected — second segment must be `outputs`
//
// The job-name charset matches gocdnext's parser idiom for job
// names (alphanumeric + dash + underscore, lower- or upper-case).
// The alias charset mirrors the parser's outputAliasRE (lowercase-
// leading, kebab/snake). Keeping them aligned means a body that
// matches the regex is GUARANTEED to be a parser-acceptable
// declaration shape — the only resolution failure left is
// "upstream didn't produce this alias".
var needsRefPattern = regexp.MustCompile(
	`^needs\.([A-Za-z0-9_-]+)\.outputs\.([a-z][a-zA-Z0-9_-]*)$`,
)

// needsRefMatrixPattern matches the matrix-selector form of a
// needs ref (issue #21):
//
//	${{ needs.bump.matrix[apac].outputs.next }}                 ← 1-dim shortcut: KEY is the dimension value
//	${{ needs.bump.matrix[shard=apac].outputs.next }}           ← explicit dim=value (1-dim)
//	${{ needs.bump.matrix[shard=apac,region=br].outputs.next }} ← explicit multi-dim
//
// The selector body (capture group 2) is the raw bracketed string
// — canonicalization (sort dims + match against the upstream's
// strategy.matrix) happens at lookup time in the scheduler, which
// has the upstream's matrix declaration to canonicalize against.
//
// Selector charset is permissive (any non-`]` char) so a value with
// `-` or `.` parses; downstream canonicalization rejects unknown
// shapes loud. Bare `${{ needs.X.outputs.Y }}` against a matrix
// upstream stays loud-error (same as v0.11.0 behaviour) so an
// operator that forgot the selector doesn't silently pick one row.
var needsRefMatrixPattern = regexp.MustCompile(
	`^needs\.([A-Za-z0-9_-]+)\.matrix\[([^\]]+)\]\.outputs\.([a-z][a-zA-Z0-9_-]*)$`,
)

// NeedsOutputs is the per-job-name table the scheduler hands the
// substitution layer at dispatch. Outer key: upstream job NAME
// (matches the YAML's `needs:` entry). Inner key: alias the
// upstream job declared in its `outputs:` block. Value: the bytes
// the upstream agent shipped in JobResult.outputs[envName].
//
// Built by the scheduler via store.ListJobOutputsForRun + the
// upstream job's declared alias→envName mapping. A nil/empty table
// passed to substituteNeedsRefs means "no needs to resolve" — the
// function fast-paths through inputs that don't contain "needs."
// anyway.
//
// **Matrix semantics** (the BUILDER enforces): a job_run row is
// classified by its `matrix_key`, not by its row count. The
// builder (scheduler.go::groupNeedsOutputs) routes:
//
//   - matrix_key=="" (non-matrix upstream) → folded into
//     NeedsOutputs[name] as the bare-ref path. Only ever fires
//     for jobs without a `strategy.matrix` block; the parser
//     rejects empty-dimension matrices at apply time so a job
//     declaring a matrix can never produce an empty-key row.
//   - matrix_key!="" (any matrix expansion, including N==1) →
//     goes into MatrixNeedsOutputs[name][canonical_key] keyed
//     by the canonical "k=v,k=v" form. Even N==1 lands here
//     because the operator declared `strategy.matrix`, so they
//     have to use the selector to be explicit. The downstream
//     substitution path looks up via the
//     `${{ needs.X.matrix[KEY].outputs.Y }}` selector
//     (issue #21). Bare `${{ needs.X.outputs.Y }}` against a
//     matrix upstream still errors loud — operators must use
//     the explicit selector.
//
// This contract keeps refs.go pure — the substitution layer
// dispatches between the two tables based on whether the body
// has a `matrix[...]` segment. All matrix policy lives in the
// layer that has the data to enforce it.
type NeedsOutputs map[string]map[string]string

// MatrixNeedsOutputs is the matrix-expanded sibling of
// NeedsOutputs (issue #21). Outer key: upstream job NAME. Middle
// key: canonical matrix-key form ("shard=apac" for 1-dim,
// "region=br,shard=apac" lex-sorted for multi-dim). Inner: alias
// → value, same shape as NeedsOutputs.
//
// Canonicalization happens at table-build time in
// groupNeedsOutputs: rows from store.JobOutputs carry the raw
// matrix_key the agent reported (e.g. "shard=apac"); the builder
// re-emits them in lex-sorted form so the lookup at substitution
// time is direct (the substitution layer also canonicalizes its
// selector body the same way).
//
// Routing is by matrix_key, not row count: any row with a
// non-empty matrix_key lands here (including N==1 matrix
// expansions, because the operator declared `strategy.matrix`
// and must use the explicit selector). Non-matrix upstreams
// (matrix_key=="") go to NeedsOutputs instead. A matrix-
// selector ref against a non-matrix upstream errors loud at
// substitution time; a bare-ref against a matrix upstream also
// errors loud — both via ErrNeedsRefUnresolved.
type MatrixNeedsOutputs map[string]map[string]map[string]string

// ErrNeedsRefUnresolved is the sentinel returned (wrapped) when a
// `${{ needs.X.outputs.Y }}` ref can't be resolved — either the
// upstream job isn't in needs:, or it ran but didn't produce the
// alias. The scheduler matches against this to terminalise the
// downstream job (failJobWithError) instead of leaving it in a
// queued retry loop: the configuration error won't fix itself on
// the next tick, so failing loud surfaces the bad reference in
// run logs with the alias name instead of an operator wondering
// "why is this job stuck?".
var ErrNeedsRefUnresolved = errors.New("needs reference unresolved")

// canonicalMatrixKey takes a selector body (the bracketed
// content of `matrix[...]`) and returns the lex-sorted
// "k=v,k=v" form for direct map lookup against
// MatrixNeedsOutputs. The dimNames slice is the upstream job's
// declared `strategy.matrix` dimension names, used to resolve
// the 1-dim shortcut (`matrix[apac]` against a 1-dim matrix
// `shard: [apac, emea]` → "shard=apac").
//
// Forms accepted:
//
//	"apac"                       (1-dim shortcut; dimNames must have len==1)
//	"shard=apac"                 (explicit, 1-dim)
//	"shard=apac,region=br"       (explicit, multi-dim; canonicalized lex-sorted)
//	"region=br, shard=apac"      (whitespace tolerated)
//
// Errors:
//
//   - 1-dim shortcut against a multi-dim upstream → ambiguous
//   - explicit k=v where k not in dimNames → unknown dimension
//   - repeated dimension in the selector → operator typo
func canonicalMatrixKey(body string, dimNames []string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("empty matrix selector body")
	}
	// 1-dim shortcut: no `=` anywhere → operator gave the value
	// of the single dimension.
	if !strings.Contains(body, "=") {
		if len(dimNames) != 1 {
			return "", fmt.Errorf(
				"1-dim shortcut %q against an upstream with %d matrix dimensions (%s); "+
					"use the explicit form matrix[k=v,...]",
				body, len(dimNames), strings.Join(dimNames, ","))
		}
		return dimNames[0] + "=" + body, nil
	}
	// Explicit form: comma-separated k=v pairs. Whitespace
	// between pairs tolerated. Each part must be `k=v`.
	dimSet := make(map[string]struct{}, len(dimNames))
	for _, d := range dimNames {
		dimSet[d] = struct{}{}
	}
	parts := strings.Split(body, ",")
	seen := make(map[string]string, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		eq := strings.IndexByte(p, '=')
		if eq <= 0 || eq == len(p)-1 {
			return "", fmt.Errorf("matrix selector part %q is not k=v", p)
		}
		k, v := p[:eq], p[eq+1:]
		if _, dup := seen[k]; dup {
			return "", fmt.Errorf("matrix selector repeats dimension %q", k)
		}
		if _, ok := dimSet[k]; !ok && len(dimNames) > 0 {
			return "", fmt.Errorf(
				"matrix selector dimension %q is not declared on the upstream's strategy.matrix (declared: %s)",
				k, strings.Join(dimNames, ","))
		}
		seen[k] = v
	}
	// Canonical form: dims in lex-sorted order, joined by `,`.
	canonKeys := make([]string, 0, len(seen))
	for k := range seen {
		canonKeys = append(canonKeys, k)
	}
	sort.Strings(canonKeys)
	var b strings.Builder
	for i, k := range canonKeys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(seen[k])
	}
	return b.String(), nil
}

// MatrixDimNames maps an upstream job NAME → the dimension names
// declared on its `strategy.matrix`. Empty/nil for jobs that aren't
// matrix expansions. Built by the scheduler at dispatch time from
// the pipeline definition snapshot; threaded into the substitution
// layer so canonicalMatrixKey can resolve the 1-dim shortcut.
type MatrixDimNames map[string][]string

// substituteNeedsRefs replaces every `${{ needs.<job>.outputs.<alias> }}`
// or `${{ needs.<job>.matrix[KEY].outputs.<alias> }}` token in s
// with the resolved value. Runs as a PRE-PASS before substituteRefs
// so the remaining body is plain identifiers.
//
// Errors are returned wrapped in ErrNeedsRefUnresolved so the
// dispatcher can terminalise the job loud (vs queued retry). Error
// shapes covered:
//
//   - bare ref to a job NOT in `needs`
//   - bare ref to an alias the upstream didn't write
//   - bare ref to a matrix-expanded upstream (use selector instead)
//   - selector ref to a non-matrix upstream
//   - selector ref with malformed body (canonicalMatrixKey errors)
//   - selector ref to an unmatched matrix key
//   - selector ref to a matched matrix key but missing alias
//
// Single pass; a resolved value containing `${{ ... }}` lands in
// the output verbatim (prevents trivially-constructed recursion).
func substituteNeedsRefs(s string, needs NeedsOutputs, matrix MatrixNeedsOutputs, dims MatrixDimNames) (string, error) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	if !strings.Contains(s, "needs.") {
		// Body might match a non-needs ref like `${{ SECRET }}` —
		// pass through so the downstream substituteRefs handles
		// it. Cheap substring check avoids re-running the regex.
		return s, nil
	}
	var (
		missingJob       []string
		missingOutput    []string
		bareOnMatrix     []string
		selectorOnPlain  []string
		selectorBadShape []string
		selectorMissKey  []string
	)
	out := refPattern.ReplaceAllStringFunc(s, func(match string) string {
		body := refPattern.FindStringSubmatch(match)[1]

		// Matrix selector takes precedence: its regex has a
		// stricter shape (the `matrix[...]` middle segment).
		if mm := needsRefMatrixPattern.FindStringSubmatch(body); mm != nil {
			jobName, selBody, alias := mm[1], mm[2], mm[3]
			// Existence in `needs`: a matrix upstream lands rows
			// in `matrix[jobName]`. A non-matrix upstream that
			// the operator typo'd with a selector lands in
			// `needs[jobName]` only → surface as selectorOnPlain.
			rows, ok := matrix[jobName]
			if !ok {
				if _, plainOK := needs[jobName]; plainOK {
					selectorOnPlain = append(selectorOnPlain, body)
					return match
				}
				missingJob = append(missingJob, body)
				return match
			}
			canon, err := canonicalMatrixKey(selBody, dims[jobName])
			if err != nil {
				selectorBadShape = append(selectorBadShape, fmt.Sprintf("%s (%s)", body, err))
				return match
			}
			rowOutputs, ok := rows[canon]
			if !ok {
				// Build the list of available keys for the error
				// message — sorted for stability.
				avail := make([]string, 0, len(rows))
				for k := range rows {
					avail = append(avail, k)
				}
				sort.Strings(avail)
				selectorMissKey = append(selectorMissKey,
					fmt.Sprintf("%s (canonical %q; available: %s)", body, canon, strings.Join(avail, " | ")))
				return match
			}
			value, ok := rowOutputs[alias]
			if !ok {
				missingOutput = append(missingOutput, body)
				return match
			}
			return value
		}

		m := needsRefPattern.FindStringSubmatch(body)
		if m == nil {
			// Not a needs ref shape — leave for substituteRefs.
			return match
		}
		jobName, alias := m[1], m[2]
		outputs, ok := needs[jobName]
		if !ok {
			// Could be a matrix upstream that the operator
			// reffed bare — distinguish for a sharper message.
			if _, isMatrix := matrix[jobName]; isMatrix {
				bareOnMatrix = append(bareOnMatrix, body)
				return match
			}
			missingJob = append(missingJob, body)
			return match
		}
		value, ok := outputs[alias]
		if !ok {
			missingOutput = append(missingOutput, body)
			return match
		}
		return value
	})
	// Error precedence: first the "missing dependency" class, then
	// the "matrix mistakes" class, then "alias not produced". This
	// ordering surfaces the most structural error first so the
	// operator fixes the obvious typo before debugging output
	// emission.
	switch {
	case len(missingJob) > 0:
		return "", fmt.Errorf(
			"%w: %s — the referenced job is not in this job's `needs:` list, "+
				"or the upstream didn't run (every needs.X.outputs.Y requires X listed in `needs:`)",
			ErrNeedsRefUnresolved,
			strings.Join(dedupeSorted(missingJob), ", "))
	case len(bareOnMatrix) > 0:
		return "", fmt.Errorf(
			"%w: %s — the upstream is a matrix that expanded to multiple instances. "+
				"Use the explicit per-row selector `${{ needs.X.matrix[k=v,...].outputs.Y }}` to pick one row",
			ErrNeedsRefUnresolved,
			strings.Join(dedupeSorted(bareOnMatrix), ", "))
	case len(selectorOnPlain) > 0:
		return "", fmt.Errorf(
			"%w: %s — the upstream is NOT a matrix; drop the matrix[...] selector and use the bare form",
			ErrNeedsRefUnresolved,
			strings.Join(dedupeSorted(selectorOnPlain), ", "))
	case len(selectorBadShape) > 0:
		return "", fmt.Errorf(
			"%w: %s — matrix selector is malformed",
			ErrNeedsRefUnresolved,
			strings.Join(dedupeSorted(selectorBadShape), "; "))
	case len(selectorMissKey) > 0:
		return "", fmt.Errorf(
			"%w: %s — the upstream matrix didn't produce a row with that key",
			ErrNeedsRefUnresolved,
			strings.Join(dedupeSorted(selectorMissKey), "; "))
	case len(missingOutput) > 0:
		return "", fmt.Errorf(
			"%w: %s — the upstream job ran but did not produce the named output. "+
				"Declare the alias under the upstream's `outputs:` block AND have its plugin write the value to $GOCDNEXT_OUTPUT_FILE",
			ErrNeedsRefUnresolved,
			strings.Join(dedupeSorted(missingOutput), ", "))
	}
	return out, nil
}

// substituteNeedsRefsMap is the map-valued lift of substituteNeedsRefs.
// Same fresh-map contract as substituteRefsMap. Empty/nil input
// passes through so callers can chain without guarding.
func substituteNeedsRefsMap(in map[string]string, needs NeedsOutputs, matrix MatrixNeedsOutputs, dims MatrixDimNames) (map[string]string, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		resolved, err := substituteNeedsRefs(v, needs, matrix, dims)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}
