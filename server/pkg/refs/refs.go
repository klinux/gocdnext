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
// isn't a closing brace) and validated downstream (bare ident, or the
// `vars.`/`secrets.` namespaces) so an unsupported body like
// `${{ foo.bar.baz }}` matches as a REF and gets a clear "unsupported
// expression" error instead of silently passing through as a literal.
// Exported because the scheduler's needs-ref pass scans the same
// `${{ ... }}` tokens.
//
// Anchored to the composite `${{ ... }}` so shell-style `${VAR}` and
// template-style `{{ X }}` alone are NOT references.
var RefPattern = regexp.MustCompile(`\$\{\{\s*([^}]+?)\s*\}\}`)

// identPattern validates a plain identifier — the whole body of a bare
// `${{ NAME }}` ref, and the NAME part after a `vars.`/`secrets.`
// prefix. Only POSIX env-var identifiers qualify; anything else (extra
// dots, brackets, operators, function calls) is the GitHub-Actions
// expression grammar gocdnext does NOT implement, and refusing it
// loudly avoids the silent-no-op trap.
var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// shellVarPattern matches `${VAR}` shell-style references — the form
// CI built-ins use (`${CI_COMMIT_SHORT_SHA}`, …) and what every Drone
// / Woodpecker / GitLab plugin recipe expects. Only identifier names
// match — `${1}`, `${PATH:-/usr}` and other bash parameter-expansion
// forms are ignored so legitimate shell syntax isn't stomped.
var shellVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// nsVars / nsSecrets are the explicit reference namespaces (#281):
// `${{ vars.NAME }}` and `${{ secrets.NAME }}`. Each resolves ONLY
// against its own source — a `vars.` ref NEVER falls back to secrets
// (the guarantee that makes `${{ vars.X }}` provably non-secret, so it
// may reach persisted/UI-shown fields like deploy.version) and a
// `secrets.` ref never falls back to variables.
const (
	nsVars    = "vars."
	nsSecrets = "secrets."
)

// Namespaces carries the explicit-namespace resolution sources for
// SubstituteRefsNS. Vars resolves `${{ vars.NAME }}`; Secrets resolves
// `${{ secrets.NAME }}`. Either may be nil (then a ref into that empty
// namespace fails as unresolved, never leaks across namespaces).
type Namespaces struct {
	Vars    map[string]string
	Secrets map[string]string
}

// SubstituteRefs replaces every `${{ NAME }}` token in s with the
// corresponding value from sources. Sources are consulted in order:
// the first hit wins (so a job-local override shadows the pipeline
// variable that shadows the project secret, etc.). This is the
// bare-identifier (legacy) form; see SubstituteRefsNS for the explicit
// `${{ vars.X }}` / `${{ secrets.X }}` namespaces.
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
	return SubstituteRefsNS(s, Namespaces{}, sources...)
}

// SubstituteRefsNS is the namespace-aware core. It resolves three ref
// shapes inside `${{ ... }}`:
//
//   - `${{ vars.NAME }}`    → ns.Vars[NAME] only (never secrets/bare).
//   - `${{ secrets.NAME }}` → ns.Secrets[NAME] only (never vars/bare).
//   - `${{ NAME }}`         → bareSources in order, first hit (legacy).
//
// Anything else inside `${{ }}` (dotted needs refs are handled by the
// scheduler's pre-pass BEFORE this; function calls, operators, nested
// dots) is an "unsupported expression" error, never a silent pass.
//
// The namespace isolation is a security property, not a convenience:
// `vars.X` resolving strictly against ns.Vars is what lets the parser
// safely allow `${{ vars.* }}` in persisted/UI fields (deploy.version)
// — a secret can never reach them through a `vars.` ref even if a
// secret of the same name exists.
//
// Single-pass + error-message-safety are identical to SubstituteRefs.
func SubstituteRefsNS(s string, ns Namespaces, bareSources ...map[string]string) (string, error) {
	if !strings.Contains(s, "${{") {
		// Fast path: no token possible, skip the regex pass.
		return s, nil
	}
	var unresolved, invalid []string
	out := RefPattern.ReplaceAllStringFunc(s, func(match string) string {
		body := RefPattern.FindStringSubmatch(match)[1]
		switch {
		case strings.HasPrefix(body, nsVars):
			name := body[len(nsVars):]
			if !identPattern.MatchString(name) {
				invalid = append(invalid, body)
				return match
			}
			if v, ok := ns.Vars[name]; ok {
				return v
			}
			unresolved = append(unresolved, body)
			return match
		case strings.HasPrefix(body, nsSecrets):
			name := body[len(nsSecrets):]
			if !identPattern.MatchString(name) {
				invalid = append(invalid, body)
				return match
			}
			if v, ok := ns.Secrets[name]; ok {
				return v
			}
			unresolved = append(unresolved, body)
			return match
		case identPattern.MatchString(body):
			for _, src := range bareSources {
				if v, ok := src[body]; ok {
					return v
				}
			}
			unresolved = append(unresolved, body)
			return match
		default:
			invalid = append(invalid, body)
			return match // keep literal so context is visible if caller logs
		}
	})
	switch {
	case len(invalid) > 0:
		return "", fmt.Errorf(
			"unsupported reference expression(s): %s — gocdnext only supports "+
				"plain identifier refs (`${{ NAME }}`) and the explicit "+
				"`${{ vars.NAME }}` / `${{ secrets.NAME }}` namespaces, not the full "+
				"Actions expression grammar (nested dots, function calls, operators)",
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
	return SubstituteRefsNSMap(in, Namespaces{}, sources...)
}

// SubstituteRefsNSMap is the map-valued lift of SubstituteRefsNS. Same
// fresh-map contract as SubstituteRefsMap.
func SubstituteRefsNSMap(in map[string]string, ns Namespaces, bareSources ...map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		resolved, err := SubstituteRefsNS(v, ns, bareSources...)
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
