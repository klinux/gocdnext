package parser

import (
	"fmt"
	"strings"
)

// resolvePluginRef translates a YAML `uses:` reference into the
// Docker image spec the runner feeds to `docker run`. Accepted
// shapes (mirrors GitHub Actions' `uses:` syntax):
//
//	owner/name                     → "owner/name" (Docker defaults :latest)
//	owner/name@v1                  → "owner/name:v1"
//	owner/name@1.2.3               → "owner/name:1.2.3"
//	owner/name@sha256:<hex>        → "owner/name@sha256:<hex>" (digest pin, passed through)
//	registry.io/owner/name@v1      → "registry.io/owner/name:v1"
//
// Tag validation is intentionally lax — Docker already rejects
// garbage tags at pull time with a clear error. We only catch
// the shapes a user would obviously typo (empty tag after @,
// whitespace) and let Docker own the rest so we don't drift
// from its rules.
func resolvePluginRef(uses string) (string, error) {
	uses = strings.TrimSpace(uses)
	if uses == "" {
		return "", fmt.Errorf("`uses:` is empty")
	}
	if strings.ContainsAny(uses, " \t\n") {
		return "", fmt.Errorf("`uses:` contains whitespace: %q", uses)
	}

	// A digest pin is the one case `@` stays intact — Docker
	// addresses blobs by digest via `image@sha256:<hex>`, same
	// punctuation as a tag version but different semantics.
	// Detect by the `sha256:` prefix after `@`; anything else
	// uses the tag rewrite.
	at := strings.Index(uses, "@")
	if at < 0 {
		return uses, nil
	}
	base, suffix := uses[:at], uses[at+1:]
	if base == "" {
		return "", fmt.Errorf("`uses:` missing image before `@`: %q", uses)
	}
	if suffix == "" {
		return "", fmt.Errorf("`uses:` missing version after `@`: %q", uses)
	}

	if strings.HasPrefix(suffix, "sha256:") {
		// Digest pin — pass through verbatim so Docker resolves
		// the exact blob regardless of what `:latest` points at
		// today. Most reliable pin; operators with a supply-chain
		// posture should prefer this over `@v1`.
		return uses, nil
	}

	// `@v1` → `:v1`. Docker reads the colon-tag form natively.
	// We could also allow the user to pass a raw `:v1` directly
	// in `uses:` but the `@` spelling matches GH Actions which
	// is what anyone coming from that ecosystem expects.
	return base + ":" + suffix, nil
}
