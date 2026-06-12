package parser

import (
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// validateTriggerPaths checks every `when.paths:` glob at parse time
// so a typo'd pattern dies in the apply, not silently at dispatch.
// Grammar is doublestar — the same the agent uses for artifacts:
// globs are workspace-relative, `**` crosses directories, no
// traversal, no absolute paths.
//
// Errors carry the pipeline name (caller wraps) + the offending glob
// verbatim, NOT the YAML line — by the time this runs, WhenDef came
// through the plain unmarshal and yaml.Node positions are gone.
// Getting line numbers would mean a manual node-walk like OutputDef's;
// not worth it while the glob text itself pinpoints the entry.
func validateTriggerPaths(paths []string) error {
	for i, g := range paths {
		if g == "" {
			return fmt.Errorf("entry %d is empty", i)
		}
		if strings.HasPrefix(g, "/") {
			return fmt.Errorf("%q is absolute — globs are repo-relative", g)
		}
		// Reject `..` as a path segment (not substrings like
		// `foo..bar`): changed-file paths from providers are always
		// repo-relative, so a traversal segment can never match and
		// only signals a misunderstanding.
		for _, seg := range strings.Split(g, "/") {
			if seg == ".." {
				return fmt.Errorf("%q contains a '..' segment — globs are repo-relative", g)
			}
		}
		if !doublestar.ValidatePattern(g) {
			return fmt.Errorf("%q is not a valid glob", g)
		}
	}
	return nil
}
