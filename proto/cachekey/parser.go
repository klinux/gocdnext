// Package cachekey parses and expands cache key templates of the
// form `prefix-{{ hash "<path>" }}-suffix`. Lives in the proto/
// module because both sides of the gRPC wire need to agree on the
// grammar — server-side parser (validateNeeds-style apply-time
// check) and agent-side runtime expansion (after checkout, before
// fetchCaches) share this code so they can never drift.
//
// Grammar (v1):
//
//	key      := (literal | template)+
//	template := "{{" ws* IDENT ws+ STRING ws* "}}"
//	IDENT    := whitelist: {"hash"}
//	STRING   := "\"" [^"]{0,255} "\""    (no escapes; v1 args are
//	            file paths which never contain quote chars)
//	literal  := anything not matching template
//
// Single-pass evaluation per CLAUDE.md: function output is hex
// (`[0-9a-f]{12}` for hash), which can never re-match template
// syntax — recursion via chain expansion is structurally
// impossible. Adding any future function with alphanumeric+
// outside the hex set MUST keep this property.
//
// All limits and the function whitelist live here as constants so
// they're a single source of truth for tests + docs.
package cachekey

import (
	"fmt"
	"regexp"
	"strings"
)

// Limits — chosen to keep the parse cost bounded for the worst
// case YAML input. Sized generously above any legitimate use
// while preventing a malformed/pathological template from
// burning regex backtracking or storing megabytes in
// job_runs.error and structured logs.
const (
	// MaxKeyLength bounds both the raw template and the expanded
	// output. 1 KiB is well above any sane key; storage backend's
	// CacheStorageKey hashes the result anyway, so length on the
	// wire isn't a constraint — this is purely about parse safety
	// and log-line cost.
	MaxKeyLength = 1024

	// MaxTokens caps how many `{{ }}` blocks a single key can
	// contain. 5 is plenty for `prefix-{{ a }}-{{ b }}-{{ c }}`
	// patterns; anything more starts being unreadable.
	MaxTokens = 5

	// MaxArgLength bounds the literal string argument inside a
	// function call. File paths fit comfortably; this stops a
	// 1 MiB YAML string from making it to the regex engine.
	MaxArgLength = 255

	// HashOutputLength is the length (in hex chars) of the hash()
	// function's output. 12 hex = 48 bits of entropy; collision
	// probability inside a single project's cache scope is
	// astronomically low (cache misses on collision just rebuild
	// — no correctness impact). Trades full-sha256 length for
	// human-readable keys in operator logs.
	HashOutputLength = 12
)

// cacheKeyTokenRE matches `{{ FN "ARG" }}` blocks. Compiled once
// at init. The quantifiers are bounded in PRACTICE by the
// MaxKeyLength cap on the raw input (checked in Parse before this
// regex runs): no quantifier can match more characters than the
// whole input, so `[^"]*` over a ≤1 KiB input scans linearly with
// no backtracking risk. CLAUDE.md's "no unbounded regex" rule is
// satisfied by the upstream input cap rather than per-quantifier
// {0,N} — gives us better error messages (explicit "argument
// length exceeded" vs silent "malformed") without expanding the
// effective worst case.
var cacheKeyTokenRE = regexp.MustCompile(
	`\{\{\s*(\w+)\s+"([^"]*)"\s*\}\}`,
)

// stragglerRE catches `{{` sequences that DIDN'T match the full
// token regex — malformed template syntax (missing function,
// missing arg, missing closing `}}`, escape attempt). Used to
// fail loudly instead of silently treating "{{ broken" as
// literal text.
var stragglerRE = regexp.MustCompile(`\{\{`)

// AllowedFunctions is the closed whitelist of template functions
// in v1. Extending it requires a PR explicitly registering the
// new name + an Expand-side implementation. Closed-list rather
// than open-namespace because each function's output is part of
// the cache key's identity — silently adding one would change
// existing key semantics.
var AllowedFunctions = map[string]bool{
	"hash": true,
}

// expandedCharsetRE matches the allowed characters in an EXPANDED
// cache key. Reached ONLY for tokenized templates (zero-token
// legacy literals take the agent-side HasTokens fast path and
// bypass Expand entirely). Defense-in-depth: parse-time charset
// + glob-arg sanitisation already constrain the inputs; this
// re-check catches a buggy function whose output strays outside
// the hex set (a future base64 / printable-ascii function would
// trip this if mis-implemented).
var expandedCharsetRE = regexp.MustCompile(`^[a-zA-Z0-9\-_.]+$`)

// literalChunkCharsetRE is the parse-time charset rule applied
// ONLY to the literal portions of a TOKENIZED template (a key
// containing at least one `{{ ... }}`). Same alphabet as
// expandedCharsetRE but allows empty — adjacent tokens like
// `{{ a }}{{ b }}` produce an empty literal chunk between them,
// which is fine. Function output is hex by design
// (cachekey.HashOutputLength chars of [0-9a-f]) so the EXPANDED
// key is guaranteed to satisfy expandedCharsetRE iff every
// literal chunk satisfies this one.
//
// CRITICAL — DOES NOT APPLY TO ZERO-TOKEN KEYS. Legacy literal
// keys like `pnpm-store-${CI_COMMIT_BRANCH}` were documented and
// recommended for years before v0.4.37; enforcing the strict
// charset on them would break every existing pipeline on agent
// upgrade. The agent re-parses every cache key on dispatch, so
// the regression would fire at runtime even without a re-apply.
//
// Storage already hashes the user-supplied key via
// CacheStorageKey(projectID, key) before deriving a blob path —
// no charset on the raw key is needed for storage safety. The
// strict rule is a NEW contract opted into by writing a
// `{{ ... }}` template; legacy keys remain opaque strings.
var literalChunkCharsetRE = regexp.MustCompile(`^[a-zA-Z0-9\-_.]*$`)

// Template is the parsed result. Carrying a parsed shape (vs
// re-parsing on each expansion) lets the server validate ONCE at
// apply time and the agent expand cheaply per dispatch. Opaque
// from the outside; consumers only call Expand.
type Template struct {
	raw   string
	parts []part
}

// Raw returns the original template string. Used by the agent's
// log statements to surface the operator-written form, not the
// expanded one, when an expansion error fires.
func (t *Template) Raw() string { return t.raw }

type part struct {
	// Exactly one of these is set:
	literal string
	fn      string
	arg     string
}

// FunctionResolver is implemented by the agent at runtime to
// turn function calls into their hex outputs. The server-side
// parser never expands, so it doesn't need a resolver — Parse
// returns a valid template; only Expand needs one.
type FunctionResolver interface {
	// Hash returns the expanded value for `{{ hash "<arg>" }}`.
	// Implementation reads files (or globs) under the workspace,
	// concatenates their content hashes in a deterministic order,
	// returns the first HashOutputLength hex chars of the result.
	// Errors propagate verbatim through Expand.
	Hash(arg string) (string, error)
}

// Parse validates a cache key template AT APPLY TIME. Called by
// the YAML parser when materialising domain.CacheSpec; rejects
// the project apply on syntax error so bad config never persists.
// The Template returned has the same string round-trip via Raw()
// — callers can keep treating cache.key as a string in the
// domain layer and only invoke Expand at agent dispatch time.
//
// Accepts a key with zero tokens (a plain literal like
// `pnpm-store-static`) — backwards compatible with pre-v0.4.37
// keys, which never expand anyway.
func Parse(raw string) (*Template, error) {
	if raw == "" {
		return nil, fmt.Errorf("cache key: empty")
	}
	if len(raw) > MaxKeyLength {
		return nil, fmt.Errorf("cache key: raw template length %d exceeds max %d", len(raw), MaxKeyLength)
	}

	t := &Template{raw: raw}
	matches := cacheKeyTokenRE.FindAllStringSubmatchIndex(raw, -1)
	if len(matches) > MaxTokens {
		return nil, fmt.Errorf("cache key: %d {{ }} tokens exceeds max %d", len(matches), MaxTokens)
	}

	// Walk matches in order, building parts. Between each token
	// (and at the head/tail) we emit literal chunks. Detect
	// malformed `{{` in literals as a hard error — silently
	// treating "{{ broken" as literal would let a user typo
	// poison the cache key without warning.
	cursor := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		fnStart, fnEnd := m[2], m[3]
		argStart, argEnd := m[4], m[5]

		if start > cursor {
			chunk := raw[cursor:start]
			if stragglerRE.MatchString(chunk) {
				idx := stragglerRE.FindStringIndex(chunk)[0]
				return nil, fmt.Errorf("cache key: malformed `{{` at offset %d (expected `{{ fn \"arg\" }}`)", cursor+idx)
			}
			if !literalChunkCharsetRE.MatchString(chunk) {
				return nil, fmt.Errorf("cache key: literal chunk %q at offset %d contains characters outside [a-zA-Z0-9-_.] (would fail at agent expansion)", chunk, cursor)
			}
			t.parts = append(t.parts, part{literal: chunk})
		}

		fn := raw[fnStart:fnEnd]
		arg := raw[argStart:argEnd]
		if !AllowedFunctions[fn] {
			return nil, fmt.Errorf("cache key: unknown function %q at offset %d (allowed: hash)", fn, start)
		}
		if len(arg) > MaxArgLength {
			return nil, fmt.Errorf("cache key: function %q argument length %d exceeds max %d", fn, len(arg), MaxArgLength)
		}
		if arg == "" {
			return nil, fmt.Errorf("cache key: function %q empty argument at offset %d", fn, start)
		}
		// Path-traversal guard. Arguments are workspace-relative
		// paths/globs; `..` could escape the workspace at expand
		// time. Reject at parse so the agent doesn't even need
		// to defend.
		if strings.Contains(arg, "..") {
			return nil, fmt.Errorf("cache key: function %q argument %q must not contain `..`", fn, arg)
		}
		// Reject absolute paths for the same reason. A literal
		// path like "/etc/passwd" would otherwise be read by the
		// agent's hash resolver.
		if strings.HasPrefix(arg, "/") {
			return nil, fmt.Errorf("cache key: function %q argument %q must be workspace-relative", fn, arg)
		}
		t.parts = append(t.parts, part{fn: fn, arg: arg})
		cursor = end
	}

	// Tail literal (if any) plus the malformed-`{{` guard. The
	// charset check is gated on whether the key contained ANY
	// token in the loop above (len(matches) > 0). Zero-token
	// keys are legacy literals like `pnpm-store-${CI_BRANCH}` —
	// keep them as opaque strings, see the literalChunkCharsetRE
	// docstring for why.
	hasTokens := len(matches) > 0
	if cursor < len(raw) {
		chunk := raw[cursor:]
		if stragglerRE.MatchString(chunk) {
			idx := stragglerRE.FindStringIndex(chunk)[0]
			return nil, fmt.Errorf("cache key: malformed `{{` at offset %d (expected `{{ fn \"arg\" }}`)", cursor+idx)
		}
		if hasTokens && !literalChunkCharsetRE.MatchString(chunk) {
			return nil, fmt.Errorf("cache key: literal chunk %q at offset %d contains characters outside [a-zA-Z0-9-_.] (templated keys require the strict charset; remove the {{...}} for legacy literal handling)", chunk, cursor)
		}
		t.parts = append(t.parts, part{literal: chunk})
	}

	return t, nil
}

// Expand evaluates the template against the resolver. Returns the
// fully-substituted cache key. Fails loud on resolver errors
// (e.g. glob matched zero files) — silent fallback to the
// unexpanded template would let two distinct lockfiles land on
// the same cache key, defeating the whole point of the feature.
//
// Post-expansion checks:
//   - Length still within MaxKeyLength (a runaway literal
//     concatenated with multiple hash outputs could theoretically
//     creep past, though MaxKeyLength on the raw template already
//     bounds it tightly).
//   - Charset matches the conservative allowed set so the storage
//     backend (filesystem/S3 prefix) never sees an unexpected
//     character.
func (t *Template) Expand(r FunctionResolver) (string, error) {
	var b strings.Builder
	b.Grow(len(t.raw))
	for _, p := range t.parts {
		if p.fn == "" {
			b.WriteString(p.literal)
			continue
		}
		switch p.fn {
		case "hash":
			v, err := r.Hash(p.arg)
			if err != nil {
				return "", fmt.Errorf("cache key %q: hash(%q): %w", t.raw, p.arg, err)
			}
			if len(v) != HashOutputLength {
				return "", fmt.Errorf("cache key %q: hash(%q) returned %d hex chars, want %d (resolver bug)", t.raw, p.arg, len(v), HashOutputLength)
			}
			b.WriteString(v)
		default:
			// Defensive: AllowedFunctions whitelist at Parse time
			// should make this unreachable. Belt-and-braces in
			// case Parse and Expand drift on a future PR.
			return "", fmt.Errorf("cache key %q: unknown function %q at expand time (Parse-Expand drift)", t.raw, p.fn)
		}
	}
	out := b.String()
	if len(out) > MaxKeyLength {
		return "", fmt.Errorf("cache key %q: expanded length %d exceeds max %d", t.raw, len(out), MaxKeyLength)
	}
	if !expandedCharsetRE.MatchString(out) {
		return "", fmt.Errorf("cache key %q: expanded value %q contains characters outside [a-zA-Z0-9-_.]", t.raw, out)
	}
	return out, nil
}

// HasTokens reports whether the template carries any function
// calls. Used by the agent to fast-path keys that never expand:
// no tokens → return the raw key without involving the
// FunctionResolver, saving the workspace filesystem walk.
func (t *Template) HasTokens() bool {
	for _, p := range t.parts {
		if p.fn != "" {
			return true
		}
	}
	return false
}
