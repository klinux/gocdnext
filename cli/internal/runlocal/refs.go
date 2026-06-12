package runlocal

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// This file mirrors server/internal/scheduler/refs.go — the dispatch
// substitution contract — byte-for-byte in behavior: strict
// `${{ NAME }}` refs error loud on unresolved/unsupported (citing
// NAMES only, never values), soft `${VAR}` refs leave unknowns
// literal. The scheduler copy is module-internal; when refs ever
// moves to server/pkg, delete this and import it. The tests in
// refs_test.go pin the parity.

var (
	refPattern      = regexp.MustCompile(`\$\{\{\s*([^}]+?)\s*\}\}`)
	identPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	shellVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

func substituteRefs(s string, sources ...map[string]string) (string, error) {
	if !strings.Contains(s, "${{") {
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
		return match
	})
	switch {
	case len(invalid) > 0:
		return "", fmt.Errorf(
			"unsupported reference expression(s): %s — gocdnext only supports "+
				"plain identifier refs (`${{ NAME }}`); `${{ needs.* }}` outputs "+
				"are not available in run-local",
			strings.Join(dedupeSorted(invalid), ", "))
	case len(unresolved) > 0:
		return "", fmt.Errorf(
			"unresolved reference(s): %s — add them to --env-file or the "+
				"pipeline's `variables:` map",
			strings.Join(dedupeSorted(unresolved), ", "))
	}
	return out, nil
}

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

func dedupeSorted(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; !dup {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
