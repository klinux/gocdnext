package main

import (
	"os"
	"testing"
)

func TestMetricsAddrThreeStates(t *testing.T) {
	const key = "GOCDNEXT_METRICS_ADDR"
	orig, had := os.LookupEnv(key)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})

	// UNSET → loopback default (never the network).
	_ = os.Unsetenv(key)
	if got := metricsAddr(); got != "127.0.0.1:9464" {
		t.Fatalf("unset → %q, want 127.0.0.1:9464", got)
	}

	// SET-EMPTY → disabled. This is what the chart writes for metrics.enabled=false,
	// and it must be distinguishable from unset (hence LookupEnv, not Getenv).
	_ = os.Setenv(key, "")
	if got := metricsAddr(); got != "" {
		t.Fatalf("empty → %q, want \"\" (disabled)", got)
	}

	// SET-VALUE → that address.
	_ = os.Setenv(key, ":9999")
	if got := metricsAddr(); got != ":9999" {
		t.Fatalf("value → %q, want :9999", got)
	}
}
