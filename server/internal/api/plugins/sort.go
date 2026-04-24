package plugins

import "sort"

// sortStrings wraps sort.Strings so every ordering call site
// in this package names the same helper — grepping "everywhere
// catalog HTTP ordering happens" becomes a single shot.
func sortStrings(s []string) { sort.Strings(s) }
