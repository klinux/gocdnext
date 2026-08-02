package parser

import (
	"strings"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// gateYAML builds a one-gate pipeline with an optional job-level `timeout:`.
func gateYAML(timeout string) string {
	line := ""
	if timeout != "" {
		line = "    timeout: " + timeout + "\n"
	}
	return `name: ci
stages: [deploy]
jobs:
  gate:
    stage: deploy
` + line + `    approval:
      approvers: [alice]
`
}

func TestParseApprovalTimeout(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		// Unset is NOT "no timeout" — it defers to the server default so an
		// operator can bound abandoned gates fleet-wide without every pipeline
		// opting in. domain.ApprovalTimeoutNever is the explicit opt-out.
		{"unset inherits the server default", "", 0},
		{"hours", "24h", 24 * time.Hour},
		{"a week, the documented default", "168h", 168 * time.Hour},
		{"minutes", "90m", 90 * time.Minute},
		{"never opts out entirely", "never", domain.ApprovalTimeoutNever},
		{"never is case-insensitive", "Never", domain.ApprovalTimeoutNever},
		{"never tolerates surrounding space", "  never  ", domain.ApprovalTimeoutNever},
		// `off` is the accepted synonym for `never` (#208) — the fleet-wide env
		// var already honours it, and the per-gate spelling now matches.
		{"off opts out like never", "off", domain.ApprovalTimeoutNever},
		{"off is case-insensitive", "OFF", domain.ApprovalTimeoutNever},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Parse(strings.NewReader(gateYAML(tt.value)), "proj", "ci")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(p.Jobs) != 1 || p.Jobs[0].Approval == nil {
				t.Fatalf("expected one approval job, got %+v", p.Jobs)
			}
			if got := p.Jobs[0].Approval.Timeout; got != tt.want {
				t.Fatalf("Approval.Timeout = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseApprovalTimeoutRejects(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantHint string
	}{
		// `0` is ambiguous — it reads as "no timeout" but collides with the
		// zero value that means "inherit". Reject it and point at `never`
		// rather than silently picking one meaning.
		{"zero is ambiguous", "0", "never"},
		{"zero with unit is ambiguous", "0s", "never"},
		{"negative", "-5m", "timeout"},
		// Below the sweep cadence the window can't be honoured with any
		// precision; above 2160h (90 days) a gate is abandoned by any
		// definition.
		{"below the floor", "10s", "minimum"},
		{"above the ceiling", "3000h", "maximum"},
		{"garbage", "soon", "timeout"},
		// Go's ParseDuration has no day unit. Callers WILL write 7d, so the
		// error has to say what to write instead — same convention the
		// GOCDNEXT_LOG_RETENTION config already documents ("168h").
		{"day suffix is not Go duration syntax", "7d", "168h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(gateYAML(tt.value)), "proj", "ci")
			if err == nil {
				t.Fatalf("expected an error for timeout %q", tt.value)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Fatalf("error %q does not mention %q", err, tt.wantHint)
			}
			// The gate NAME may appear; the raw value must not leak into an
			// error that ends up in logs verbatim beyond the quoted echo.
			if !strings.Contains(err.Error(), "gate") {
				t.Fatalf("error %q should cite the job name", err)
			}
		})
	}
}

// A non-approval job's `timeout:` is parsed but not yet enforced (execution
// timeout is a separate feature — the agent path still ships TimeoutSeconds: 0).
// Asserted so this PR is provably a no-op for those pipelines rather than
// silently changing them.
func TestParseTimeoutOnNonApprovalJobIsAccepted(t *testing.T) {
	yaml := `name: ci
stages: [build]
jobs:
  compile:
    stage: build
    image: golang:1.23
    timeout: 30m
    script: [go build ./...]
`
	p, err := Parse(strings.NewReader(yaml), "proj", "ci")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Jobs) != 1 {
		t.Fatalf("expected one job, got %d", len(p.Jobs))
	}
	if p.Jobs[0].Approval != nil {
		t.Fatalf("compile should not be an approval gate")
	}
}

// Emit round-trip. The reconstructed YAML the UI shows must carry the gate's
// window, or a copy-paste-reapply silently converts an explicit `never` (or an
// explicit short window) into "inherit the server default" — a semantic change
// that turns a deliberately-unbounded compliance gate into one that expires.
//
// emit.go already carries a comment about exactly this class of bug for
// Required/ApproverGroups; Timeout is the same trap.
func TestEmitRoundTripsApprovalTimeout(t *testing.T) {
	for _, value := range []string{"never", "24h", "168h"} {
		t.Run(value, func(t *testing.T) {
			first, err := Parse(strings.NewReader(gateYAML(value)), "proj", "ci")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			out, err := Emit(first)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			again, err := Parse(strings.NewReader(string(out)), "proj", "ci")
			if err != nil {
				t.Fatalf("reparse emitted yaml: %v\n---\n%s", err, out)
			}
			want := first.Jobs[0].Approval.Timeout
			got := again.Jobs[0].Approval.Timeout
			if got != want {
				t.Fatalf("round-trip lost the window: got %v, want %v\nemitted:\n%s", got, want, out)
			}
		})
	}
}

// A gate that never declared a window must NOT gain one through the
// round-trip: emitting `timeout: 0s` would flip "inherit" into an explicit
// value the parser then rejects as ambiguous.
func TestEmitOmitsUnsetApprovalTimeout(t *testing.T) {
	p, err := Parse(strings.NewReader(gateYAML("")), "proj", "ci")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Emit(p)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if strings.Contains(string(out), "timeout:") {
		t.Fatalf("emitted a timeout key for a gate that never set one:\n%s", out)
	}
	again, err := Parse(strings.NewReader(string(out)), "proj", "ci")
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if got := again.Jobs[0].Approval.Timeout; got != 0 {
		t.Fatalf("Timeout = %v, want 0 (inherit)", got)
	}
}
