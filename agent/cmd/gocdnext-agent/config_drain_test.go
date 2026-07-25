package main

import (
	"testing"
	"time"
)

// TestLoadConfigDrainEnvs locks the drain-knob parsing contract (#178): a Go
// duration for the budget where <=0 is a VALID opt-out (no job wait), and a
// strictly-positive duration for the flush timeout where <=0 is an ERROR (a
// zero-length flush would abandon every buffered result). Malformed values on
// either fail loud rather than silently defaulting.
func TestLoadConfigDrainEnvs(t *testing.T) {
	tests := []struct {
		name       string
		budget     string // "" means leave unset
		flush      string
		wantBudget time.Duration
		wantFlush  time.Duration
		wantErr    bool
	}{
		{name: "defaults when unset", wantBudget: 5 * time.Minute, wantFlush: 45 * time.Second},
		{name: "explicit budget+flush", budget: "300s", flush: "30s", wantBudget: 300 * time.Second, wantFlush: 30 * time.Second},
		{name: "minutes form", budget: "2m", flush: "1m", wantBudget: 2 * time.Minute, wantFlush: time.Minute},
		{name: "budget zero is opt-out", budget: "0", wantBudget: 0, wantFlush: 45 * time.Second},
		{name: "budget negative is opt-out", budget: "-1s", wantBudget: -time.Second, wantFlush: 45 * time.Second},
		{name: "budget malformed errors", budget: "soon", wantErr: true},
		{name: "flush zero errors", flush: "0", wantErr: true},
		{name: "flush negative errors", flush: "-5s", wantErr: true},
		{name: "flush malformed errors", flush: "later", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Minimum required config so loadConfig reaches the drain-parse branch.
			t.Setenv("GOCDNEXT_SERVER_ADDR", "localhost:9000")
			t.Setenv("GOCDNEXT_AGENT_NAME", "agent-1")
			t.Setenv("GOCDNEXT_AGENT_TOKEN", "tok")
			if tt.budget != "" {
				t.Setenv("GOCDNEXT_DRAIN_BUDGET", tt.budget)
			}
			if tt.flush != "" {
				t.Setenv("GOCDNEXT_DRAIN_FLUSH_TIMEOUT", tt.flush)
			}

			cfg, err := loadConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("loadConfig() = nil error, want error for budget=%q flush=%q", tt.budget, tt.flush)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig() unexpected error: %v", err)
			}
			if cfg.DrainBudget != tt.wantBudget {
				t.Errorf("DrainBudget = %v, want %v", cfg.DrainBudget, tt.wantBudget)
			}
			if cfg.FlushTimeout != tt.wantFlush {
				t.Errorf("FlushTimeout = %v, want %v", cfg.FlushTimeout, tt.wantFlush)
			}
		})
	}
}
