package scheduler

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// refPattern matches GitHub-Actions / Drone-style references:
// `${{ <body> }}`. The body is captured permissively (anything that
// isn't a closing brace) and validated by identPattern below so a
// value like `${{ secrets.X }}` matches as a REF and gets a clear
// "unsupported expression" error instead of silently passing through
// as a literal — which is what a too-strict character class would
// have allowed.
//
// Anchored to the composite `${{ ... }}` so shell-style `${VAR}` and
// template-style `{{ X }}` alone are NOT references.
var refPattern = regexp.MustCompile(`\$\{\{\s*([^}]+?)\s*\}\}`)

// identPattern validates the BODY of a `${{ ... }}` reference. Only
// POSIX env-var identifiers (`[A-Za-z_][A-Za-z0-9_]*`) qualify;
// anything else (dots, brackets, operators, function calls) is the
// GitHub-Actions expression grammar that gocdnext does NOT implement,
// and refusing it loudly avoids the silent-no-op trap.
var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// shellVarPattern matches `${VAR}` shell-style references — the form
// CI built-ins use (`${CI_COMMIT_SHORT_SHA}`, `${CI_BRANCH}`, …) and
// what every Drone / Woodpecker / GitLab plugin recipe expects. We
// substitute these at dispatch time for plugin Settings (the agent
// hands the literal value to the plugin's ENTRYPOINT, no shell
// expansion happens at runtime) — but NOT for script env: a script
// using `${HOME}` or `${PATH}` should still get the runtime
// expansion bash provides natively.
//
// Only identifier names match — `${1}`, `${PATH:-/usr}` and other
// bash parameter-expansion forms are ignored so legitimate shell
// syntax in setting values doesn't get accidentally stomped.
var shellVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// substituteRefs replaces every `${{ NAME }}` token in s with the
// corresponding value from sources. Sources are consulted in order:
// the first hit wins (so a job-local override shadows the pipeline
// variable that shadows the project secret, etc.).
//
// Errors when ANY reference resolves to no source. This is the
// gocdnext contract: unresolved references must NOT silently pass
// through to the container, because the operator would only catch
// the failure inside the plugin (as we hit with v0.4.7 → buildx
// "unauthorized: authentication required" when the literal
// `${{ DOCKER_USERNAME }}` reached `docker login`). Failing fast at
// dispatch surfaces the missing-declaration in the run log with the
// reference name, not a downstream auth error.
//
// Single-pass on purpose: if a resolved value itself contains a
// `${{ NAME }}` token, it lands in the output verbatim. Prevents
// trivially-constructed recursion (`A=${{ B }}`, `B=${{ A }}`) and
// keeps the function O(n) in the number of references.
//
// Security: error messages cite the unresolved NAME only — never
// any other source's resolved value — so an exception text doesn't
// become a side channel for the secret next to the typo.
func substituteRefs(s string, sources ...map[string]string) (string, error) {
	if !strings.Contains(s, "${{") {
		// Fast path: no token possible, skip the regex pass. Most
		// values in a typical assignment don't carry refs, and the
		// scheduler runs this on every Plugin.Settings + Env value
		// of every dispatched job.
		return s, nil
	}
	var unresolved, invalid []string
	out := refPattern.ReplaceAllStringFunc(s, func(match string) string {
		body := refPattern.FindStringSubmatch(match)[1]
		if !identPattern.MatchString(body) {
			invalid = append(invalid, body)
			return match
		}
		for _, src := range sources {
			if v, ok := src[body]; ok {
				return v
			}
		}
		unresolved = append(unresolved, body)
		return match // keep literal so context is visible if caller logs
	})
	switch {
	case len(invalid) > 0:
		return "", fmt.Errorf(
			"unsupported reference expression(s): %s — gocdnext only supports "+
				"plain identifier refs (`${{ NAME }}`), not the full Actions "+
				"expression grammar (`secrets.X.Y`, function calls, operators)",
			strings.Join(dedupeSorted(invalid), ", "))
	case len(unresolved) > 0:
		return "", fmt.Errorf(
			"unresolved reference(s): %s — declare them under the job's `secrets:` list "+
				"or the pipeline's `variables:` map",
			strings.Join(dedupeSorted(unresolved), ", "))
	}
	return out, nil
}

// substituteShellVars replaces every `${VAR}` token whose name is
// present in one of the sources. Unknown names are LEFT LITERAL —
// the opposite of substituteRefs' hard-fail contract. The rationale:
// `${VAR}` is shell syntax and may legitimately appear in plugin
// settings as a runtime placeholder the operator wants the inner
// shell to expand (e.g. `script: 'echo ${HOME}'` running on an
// agent's container). Substituting at dispatch when we know the
// value, leaving alone when we don't, lets both styles coexist.
//
// Strict mode (hard-fail) lives on `${{ NAME }}` because that form
// is the gocdnext-specific contract: the operator MUST have
// declared the name.
func substituteShellVars(s string, sources ...map[string]string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return shellVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := shellVarPattern.FindStringSubmatch(match)[1]
		for _, src := range sources {
			if v, ok := src[name]; ok {
				return v
			}
		}
		return match
	})
}

// substituteShellVarsMap is the map-valued lift of substituteShellVars.
// Same fresh-map / nil-passes-through contract as substituteRefsMap.
func substituteShellVarsMap(in map[string]string, sources ...map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = substituteShellVars(v, sources...)
	}
	return out
}

// substituteRefsMap is the map-valued lift of substituteRefs. Walks
// values, leaves keys untouched (secret/variable NAMES are not
// templated — only their CONSUMERS). Returns a fresh map so the
// caller can swap it in without touching the original (which may be
// shared with other goroutines or the cached pipeline definition).
func substituteRefsMap(in map[string]string, sources ...map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		resolved, err := substituteRefs(v, sources...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}

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
// **Matrix semantics** (the BUILDER must enforce, not this map):
// a matrix job expands into N job_run rows that share the same
// `name` but differ in `matrix_key`. The downstream substitution
// path can't pick "the right one" — the operator's intent is
// inherently ambiguous when N>1. The builder (scheduler.go) MUST:
//
//   - fold a single matrix_key='' row (non-matrix job, or matrix
//     that didn't expand) into NeedsOutputs[name] as-is;
//   - error LOUD when N>1 rows share a name, citing the matrix
//     keys, BEFORE building the map. The downstream dispatch
//     never sees an ambiguous reference because we refused to
//     produce one. Explicit per-row selector
//     (`${{ needs.X.matrix[key].outputs.Y }}`) is roadmap; for
//     v1 the answer to "I have a matrix upstream" is "split the
//     work into a non-matrix output job, OR open #10 follow-up".
//
// This contract keeps refs.go pure — the substitution layer only
// has to handle the "name → outputs" lookup. All matrix policy
// lives in the layer that has the data to enforce it.
type NeedsOutputs map[string]map[string]string

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

// substituteNeedsRefs replaces every `${{ needs.<job>.outputs.<alias> }}`
// token in s with the resolved value from needs. Runs as a
// PRE-PASS before substituteRefs so the remaining body is plain
// identifiers — keeps the two resolution algorithms (structured
// lookup vs flat env) cleanly separated.
//
// Errors when:
//   - reference points at a job NOT in `needs` (the table is empty
//     for that key — operator declared the ref without the
//     dependency)
//   - reference points at an alias the upstream didn't write (job
//     ran but produced no value under that key)
//   - upstream job is a matrix that expanded to >1 instance and
//     its outputs differ (today's scope: ambiguous matrix → error;
//     explicit per-row selector is roadmap)
//
// Single pass; a resolved value containing `${{ ... }}` lands in
// the output verbatim (prevents trivially-constructed recursion).
func substituteNeedsRefs(s string, needs NeedsOutputs) (string, error) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	if !strings.Contains(s, "needs.") {
		// Body might match a non-needs ref like `${{ SECRET }}` —
		// pass through so the downstream substituteRefs handles
		// it. Cheap substring check avoids re-running the regex.
		return s, nil
	}
	var missingJob, missingOutput []string
	out := refPattern.ReplaceAllStringFunc(s, func(match string) string {
		body := refPattern.FindStringSubmatch(match)[1]
		m := needsRefPattern.FindStringSubmatch(body)
		if m == nil {
			// Not a needs ref shape — leave for substituteRefs.
			// (Could be `${{ SECRET }}`, or a malformed body that
			// substituteRefs will reject with its own error.)
			return match
		}
		jobName, alias := m[1], m[2]
		outputs, ok := needs[jobName]
		if !ok {
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
	switch {
	case len(missingJob) > 0:
		return "", fmt.Errorf(
			"%w: %s — the referenced job is not in this job's `needs:` list, "+
				"or the upstream didn't run (every needs.X.outputs.Y requires X listed in `needs:`)",
			ErrNeedsRefUnresolved,
			strings.Join(dedupeSorted(missingJob), ", "))
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
func substituteNeedsRefsMap(in map[string]string, needs NeedsOutputs) (map[string]string, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		resolved, err := substituteNeedsRefs(v, needs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}

// dedupeSorted returns a deterministic deduplicated copy. The error
// message in substituteRefs needs stable ordering so test fixtures
// don't flap on regex-replace iteration order (Go's map iteration is
// randomised but ReplaceAllStringFunc iterates left-to-right, so the
// raw list is already stable — sort defends against future refactors
// using a map-based scan).
func dedupeSorted(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
