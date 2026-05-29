package scheduler

import (
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
