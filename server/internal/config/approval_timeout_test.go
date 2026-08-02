package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/config"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// The fleet-wide default is the whole reason this feature exists: an abandoned
// approval gate must stop sitting in `running` forever WITHOUT every pipeline
// author remembering to set a window.
func TestLoad_ApprovalDefaultTimeout(t *testing.T) {
	const week = 168 * time.Hour

	tests := []struct {
		name string
		val  string
		want time.Duration
	}{
		{"unset defaults to a week", "", week},
		{"explicit window", "24h", 24 * time.Hour},
		// The kill switch has to exist: an operator who wants the old
		// wait-forever behaviour must be able to say so without editing
		// every pipeline.
		{"never disables fleet-wide", "never", 0},
		{"off disables fleet-wide", "off", 0},
		{"zero disables fleet-wide", "0", 0},
		// Any spelling of zero must disable, not clamp. "0s" parses as a
		// valid Go duration, so a naive raw=="0" check lets it fall through
		// to the range clamp and silently become a ONE MINUTE fleet-wide
		// window — an operator trying to turn expiry off would instead
		// auto-cancel every pending gate within the minute.
		{"0s disables fleet-wide", "0s", 0},
		{"0h disables fleet-wide", "0h", 0},
		{"0m0s disables fleet-wide", "0m0s", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOCDNEXT_DATABASE_URL", "postgres://x")
			if tt.val != "" {
				t.Setenv("GOCDNEXT_APPROVAL_DEFAULT_TIMEOUT", tt.val)
			}
			c, err := config.Load()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if c.ApprovalDefaultTimeout != tt.want {
				t.Fatalf("ApprovalDefaultTimeout = %v, want %v", c.ApprovalDefaultTimeout, tt.want)
			}
		})
	}
}

// A typo must abort boot rather than silently reverting to the 7d default:
// the operator who set this env var had an intent, and quietly substituting a
// different window is exactly the silent-wrong-value class the repo's posture
// rejects.
func TestLoad_ApprovalDefaultTimeout_FailFast(t *testing.T) {
	for _, val := range []string{"banana", "-1h", "7d"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("GOCDNEXT_DATABASE_URL", "postgres://x")
			t.Setenv("GOCDNEXT_APPROVAL_DEFAULT_TIMEOUT", val)
			_, err := config.Load()
			if err == nil {
				t.Fatalf("%q must fail boot, got nil", val)
			}
			if !strings.Contains(err.Error(), "GOCDNEXT_APPROVAL_DEFAULT_TIMEOUT") {
				t.Fatalf("error %q should cite the env var", err)
			}
		})
	}
}

// Out-of-range values clamp rather than fail — unlike a typo, "30s" or "1000d"
// is an unambiguous intent that just lands outside what the sweep can honour,
// and the same bounds the parser enforces on per-gate windows apply here.
func TestLoad_ApprovalDefaultTimeout_Clamps(t *testing.T) {
	tests := []struct {
		val  string
		want time.Duration
	}{
		{"10s", domain.ApprovalTimeoutMin},
		{"5000h", domain.ApprovalTimeoutMax},
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv("GOCDNEXT_DATABASE_URL", "postgres://x")
			t.Setenv("GOCDNEXT_APPROVAL_DEFAULT_TIMEOUT", tt.val)
			c, err := config.Load()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if c.ApprovalDefaultTimeout != tt.want {
				t.Fatalf("ApprovalDefaultTimeout = %v, want %v", c.ApprovalDefaultTimeout, tt.want)
			}
		})
	}
}
