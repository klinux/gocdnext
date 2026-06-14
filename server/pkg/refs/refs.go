// Package refs is the single source of truth for gocdnext's
// `${{ NAME }}` / `${VAR}` substitution. It lives in pkg/ (not
// internal/scheduler) so BOTH the scheduler's real dispatch path and
// the CLI's run-local simulator resolve references identically — the
// runlocal copy that used to drift from the scheduler is gone (#44).
//
// Scope is deliberately the GENERIC core: the strict `${{ NAME }}`
// pass, the soft `${VAR}` shell pass, and their map lifts. The
// needs-output refs (`${{ needs.X.outputs.Y }}`) and the deploy
// sentinels stay in the scheduler — they depend on dispatch-only
// context (upstream outputs, matrix dims) the run-local path doesn't
// have.
package refs

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// RefPattern matches GitHub-Actions / Drone-style references:
// `${{ <body> }}`. The body is captured permissively (anything that
// isn't a closing brace) and validated by identPattern so a value like
// `${{ secrets.X }}` matches as a REF and gets a clear "unsupported
// expression" error instead of silently passing through as a literal.
// Exported because the scheduler's needs-ref pass scans the same
// `${{ ... }}` tokens.
//
// Anchored to the composite `${{ ... }}` so shell-style `${VAR}` and
// template-style `{{ X }}` alone are NOT references.
var RefPattern = regexp.MustCompile(`\$\{\{\s*([^}]+?)\s*\}\}`)

// identPattern validates the BODY of a `${{ ... }}` reference. Only
// POSIX env-var identifiers qualify; anything else (dots, brackets,
// operators, function calls) is the GitHub-Actions expression grammar
// gocdnext does NOT implement, and refusing it loudly avoids the
// silent-no-op trap.
var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// shellVarPattern matches `${VAR}` shell-style references — the form
// CI built-ins use (`${CI_COMMIT_SHORT_SHA}`, …) and what every Drone
// / Woodpecker / GitLab plugin recipe expects. Only identifier names
// match — `${1}`, `${PATH:-/usr}` and other bash parameter-expansion
// forms are ignored so legitimate shell syntax isn't stomped.
var shellVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// SubstituteRefs replaces every `${{ NAME }}` token in s with the
// corresponding value from sources. Sources are consulted in order:
// the first hit wins (so a job-local override shadows the pipeline
// variable that shadows the project secret, etc.).
//
// Errors when ANY reference resolves to no source. This is the
// gocdnext contract: unresolved references must NOT silently pass
// through to the container, because the operator would only catch the
// failure inside the plugin. Failing fast at dispatch surfaces the
// missing declaration in the run log with the reference name, not a
// downstream auth error.
//
// Single-pass on purpose: a resolved value that itself contains a
// `${{ NAME }}` token lands in the output verbatim. Prevents
// trivially-constructed recursion and keeps it O(n).
//
// Security: error messages cite the unresolved NAME only — never any
// other source's resolved value — so the text isn't a side channel for
// the secret next to the typo.
func SubstituteRefs(s string, sources ...map[string]string) (string, error) {
	if !strings.Contains(s, "${{") {
		// Fast path: no token possible, skip the regex pass.
		return s, nil
	}
	var unresolved, invalid []string
	out := RefPattern.ReplaceAllStringFunc(s, func(match string) string {
		body := RefPattern.FindStringSubmatch(match)[1]
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
			strings.Join(DedupeSorted(invalid), ", "))
	case len(unresolved) > 0:
		return "", fmt.Errorf(
			"unresolved reference(s): %s — declare them under the job's `secrets:` "+
				"list or the pipeline's `variables:` map (run-local: pass them via "+
				"--env-file)",
			strings.Join(DedupeSorted(unresolved), ", "))
	}
	return out, nil
}

// SubstituteShellVars replaces every `${VAR}` token whose name is
// present in one of the sources. Unknown names are LEFT LITERAL — the
// opposite of SubstituteRefs' hard-fail contract: `${VAR}` is shell
// syntax that may legitimately appear in a setting as a runtime
// placeholder the inner shell expands. Substituting at dispatch when we
// know the value, leaving alone when we don't, lets both styles coexist.
func SubstituteShellVars(s string, sources ...map[string]string) string {
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

// SubstituteShellVarsMap is the map-valued lift of SubstituteShellVars.
// Fresh map; nil/empty passes through.
func SubstituteShellVarsMap(in map[string]string, sources ...map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = SubstituteShellVars(v, sources...)
	}
	return out
}

// SubstituteRefsMap is the map-valued lift of SubstituteRefs. Walks
// values, leaves keys untouched (NAMES aren't templated, only their
// consumers). Returns a fresh map so the caller can swap it in without
// touching the original (which may be shared).
func SubstituteRefsMap(in map[string]string, sources ...map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		resolved, err := SubstituteRefs(v, sources...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}

// DedupeSorted returns a deterministic deduplicated copy — exported so
// the scheduler's needs-ref error formatting shares the exact dedup.
func DedupeSorted(in []string) []string {
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
