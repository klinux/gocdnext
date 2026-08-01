package parser

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// approvalTimeoutNeverLiteral is the YAML spelling of "this gate must never
// expire", the explicit opt-out from the server-wide default.
const approvalTimeoutNeverLiteral = "never"

// daySuffixRE recognises the `7d` / `1.5d` shapes Go's ParseDuration rejects,
// so the error can name the fix instead of echoing "unknown unit d". Digit
// runs are explicitly bounded — an unbounded quantifier here would be a
// pathological-input surface on a value that comes straight from user YAML.
// Compiled once at init, never per call.
var daySuffixRE = regexp.MustCompile(`^[0-9]{1,9}(\.[0-9]{1,9})?[dD]$`)

// parseApprovalTimeout lowers a gate's job-level `timeout:` into the duration
// domain.ApprovalSpec carries. Empty yields 0 — "inherit the server default",
// NOT "no timeout"; only the `never` literal opts a gate out.
//
// Every rejection path names the gate and echoes the offending value quoted,
// so a misconfigured window fails at apply time instead of silently becoming
// an unbounded wait. jobName is the pipeline author's own identifier, quoted
// with %q like the surrounding validators.
func parseApprovalTimeout(jobName, raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}
	if strings.EqualFold(trimmed, approvalTimeoutNeverLiteral) {
		return domain.ApprovalTimeoutNever, nil
	}

	d, err := time.ParseDuration(trimmed)
	if err != nil {
		// Go's ParseDuration has no day unit, and `7d` is the single most
		// natural thing to write for an approval window — so catch it by
		// shape and say what to write instead, rather than surfacing Go's
		// "unknown unit d" which tells the author nothing actionable. Same
		// hours convention GOCDNEXT_LOG_RETENTION already documents.
		if daySuffixRE.MatchString(trimmed) {
			return 0, fmt.Errorf(
				"job %q: approval timeout %q uses a day unit — write it in hours instead (%q for 7 days)",
				jobName, trimmed, "168h")
		}
		return 0, fmt.Errorf(
			"job %q: approval timeout %q is not a duration — use Go syntax (%q, %q, %q for 7 days) or %q",
			jobName, trimmed, "90m", "24h", "168h", approvalTimeoutNeverLiteral)
	}

	switch {
	case d == 0:
		// Ambiguous on purpose: `0` reads as "no timeout" but collides with
		// the zero value that means "inherit the server default". Refuse to
		// guess — the author gets one unambiguous spelling for each intent.
		return 0, fmt.Errorf(
			"job %q: approval timeout %q is ambiguous — use %q to wait indefinitely, or omit the key to inherit the server default",
			jobName, trimmed, approvalTimeoutNeverLiteral)
	case d < 0:
		return 0, fmt.Errorf(
			"job %q: approval timeout %q is negative", jobName, trimmed)
	case d < domain.ApprovalTimeoutMin:
		return 0, fmt.Errorf(
			"job %q: approval timeout %q is below the minimum of %s — a shorter window cannot be honoured with any precision",
			jobName, trimmed, domain.ApprovalTimeoutMin)
	case d > domain.ApprovalTimeoutMax:
		return 0, fmt.Errorf(
			"job %q: approval timeout %q exceeds the maximum of %s — use %q for a gate that must wait indefinitely",
			jobName, trimmed, domain.ApprovalTimeoutMax, approvalTimeoutNeverLiteral)
	}
	return d, nil
}

// emitApprovalTimeout renders a gate's window back into the YAML `timeout:`
// scalar, inverting parseApprovalTimeout. Returns "" for the zero value so the
// key is OMITTED for a gate that inherits the server default — emitting an
// explicit value there would change the semantics of the reconstructed YAML.
func emitApprovalTimeout(d time.Duration) string {
	switch {
	case d == domain.ApprovalTimeoutNever:
		return approvalTimeoutNeverLiteral
	case d > 0:
		return d.String()
	default:
		return "" // inherit — omitempty drops the key
	}
}
