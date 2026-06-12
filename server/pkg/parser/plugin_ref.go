package parser

import "github.com/gocdnext/gocdnext/server/internal/domain"

// resolvePluginRef is a thin parser-local alias of the canonical
// resolver in domain. The domain copy has to exist because the
// store also uses it to materialize synthetic notification jobs;
// keeping a package-local name here preserves call-site terseness
// without duplicating the logic.
func resolvePluginRef(uses string) (string, error) {
	return domain.ResolvePluginRef(uses)
}
