package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/config"
)

// TestLoad_SecretDurations_FailFast: the cache TTL and fetch timeout are
// security/availability knobs — a typo or negative value must abort boot, not
// silently fall back to the default (a silent fallback could, e.g., re-enable
// a cache that was meant to be disabled).
func TestLoad_SecretDurations_FailFast(t *testing.T) {
	bad := []struct {
		name string
		key  string
		val  string
		cite string
	}{
		{"ttl garbage", "GOCDNEXT_SECRET_CACHE_TTL", "banana", "GOCDNEXT_SECRET_CACHE_TTL"},
		{"ttl negative", "GOCDNEXT_SECRET_CACHE_TTL", "-5s", "GOCDNEXT_SECRET_CACHE_TTL"},
		{"timeout garbage", "GOCDNEXT_SECRET_FETCH_TIMEOUT", "soon", "GOCDNEXT_SECRET_FETCH_TIMEOUT"},
		{"timeout negative", "GOCDNEXT_SECRET_FETCH_TIMEOUT", "-1s", "GOCDNEXT_SECRET_FETCH_TIMEOUT"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOCDNEXT_DATABASE_URL", "postgres://x") // satisfy the later required check
			t.Setenv(tt.key, tt.val)
			_, err := config.Load()
			if err == nil {
				t.Fatalf("%s=%q must fail boot, got nil", tt.key, tt.val)
			}
			if !strings.Contains(err.Error(), tt.cite) {
				t.Fatalf("error %q should cite %s", err, tt.cite)
			}
		})
	}
}

// TestLoad_SecretDurations_Defaults: unset → sane defaults; valid → parsed; 0
// is accepted (disables the cache / per-fetch timeout).
func TestLoad_SecretDurations_Defaults(t *testing.T) {
	t.Setenv("GOCDNEXT_DATABASE_URL", "postgres://x")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SecretExternalCacheTTL != 60*time.Second {
		t.Errorf("default cache ttl = %s, want 60s", cfg.SecretExternalCacheTTL)
	}
	if cfg.SecretExternalTimeout != 10*time.Second {
		t.Errorf("default fetch timeout = %s, want 10s", cfg.SecretExternalTimeout)
	}

	t.Setenv("GOCDNEXT_SECRET_CACHE_TTL", "0")
	t.Setenv("GOCDNEXT_SECRET_FETCH_TIMEOUT", "0")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("load with zeros: %v", err)
	}
	if cfg.SecretExternalCacheTTL != 0 || cfg.SecretExternalTimeout != 0 {
		t.Errorf("zero durations not honoured: ttl=%s timeout=%s", cfg.SecretExternalCacheTTL, cfg.SecretExternalTimeout)
	}
}
