package grpcsrv

import (
	"fmt"
	"regexp"
	"unicode/utf8"
)

// outputsServerCapBytes is the server-side hard cap on a JobResult's
// total outputs payload — sum of (key+value) over all entries. Set
// equal to the agent-side cap (64KB; see
// agent/internal/runner/outputs.go::outputsCapBytes) so a well-
// behaved agent never trips the server validator, but a malicious /
// buggy custom gRPC client can't bypass.
//
// Defence in depth on TWO surfaces: (1) keeps the JobAssignment-
// response bytes bounded when we eventually round-trip outputs
// downstream, (2) bounds the JSONB row width on job_runs so a
// hostile agent can't grow a column into a problem.
const outputsServerCapBytes = 64 * 1024

// outputsServerMaxEntries mirrors the parser's per-job cap (see
// parse.go::validateOutputsDeclaration). Operator-declared outputs
// max out at 64 entries; an agent shipping more is either bugged
// or hostile.
const outputsServerMaxEntries = 64

// outputsAliasRE accepts the same alias charset the parser
// enforces on the YAML side (parse.go::outputAliasRE). Resolution
// is case-sensitive end-to-end: a value the agent ships under
// `Next` does NOT match a parser-declared `next`, so requiring
// lowercase-leading here keeps the contract uniform.
var outputsAliasRE = regexp.MustCompile(`^[a-z][a-zA-Z0-9_-]*$`)

// validateJobOutputs enforces the server-side contract on a
// JobResult's outputs map. Empty/nil is the common case (most
// jobs don't declare outputs) — fast-pathed.
//
// Failures:
//   - too many entries (> outputsServerMaxEntries) → cap error
//   - any entry's alias fails outputsAliasRE → bad-shape error
//   - any value is not valid UTF-8 → binary-payload error (the
//     proto wire is bytes-permissive; the parse-time validator
//     declared values as strings; refuse smuggled binary)
//   - total bytes > outputsServerCapBytes → size error
//
// Returns nil on the empty / valid path. Caller responsibility
// to react (downgrade + strip) — the function is policy-free.
func validateJobOutputs(outputs map[string]string) error {
	if len(outputs) == 0 {
		return nil
	}
	if n := len(outputs); n > outputsServerMaxEntries {
		return fmt.Errorf("%d entries shipped, server cap is %d", n, outputsServerMaxEntries)
	}
	totalBytes := 0
	for alias, value := range outputs {
		if !outputsAliasRE.MatchString(alias) {
			return fmt.Errorf("alias %q does not match the YAML declaration shape (lowercase-leading, no shell metas)", alias)
		}
		if !utf8.ValidString(value) {
			// Don't echo the value — it's not text.
			return fmt.Errorf("value for alias %q is not valid UTF-8 (refusing binary outputs)", alias)
		}
		totalBytes += len(alias) + len(value)
		if totalBytes > outputsServerCapBytes {
			return fmt.Errorf("total bytes > %d (cap) — split large blobs into artifacts instead", outputsServerCapBytes)
		}
	}
	return nil
}
