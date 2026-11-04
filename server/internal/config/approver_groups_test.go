package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/config"
)

func TestLoad_ApproverGroupSource(t *testing.T) {
	const token = "platform-token-value"
	tests := []struct {
		name        string
		url, tok    string
		timeout     string
		wantURL     string
		wantTimeout time.Duration
		wantErr     string
	}{
		{name: "unset keeps the source off", wantTimeout: 2 * time.Second},
		{name: "configured", url: "https://zen.example.com/groups", tok: token,
			wantURL: "https://zen.example.com/groups", wantTimeout: 2 * time.Second},
		{name: "url trimmed", url: "  https://zen.example.com/groups ", tok: token,
			wantURL: "https://zen.example.com/groups", wantTimeout: 2 * time.Second},
		{name: "explicit timeout", url: "https://zen.example.com/groups", tok: token, timeout: "500ms",
			wantURL: "https://zen.example.com/groups", wantTimeout: 500 * time.Millisecond},
		// An unauthenticated lookup would be refused anyway; catch it at boot, not per vote.
		{name: "url without token aborts", url: "https://zen.example.com/groups",
			wantErr: "GOCDNEXT_APPROVER_GROUPS_TOKEN"},
		{name: "bad timeout aborts", url: "https://zen.example.com/groups", tok: token, timeout: "soon",
			wantErr: "GOCDNEXT_APPROVER_GROUPS_TIMEOUT"},
		{name: "timeout below range aborts", url: "https://zen.example.com/groups", tok: token, timeout: "10ms",
			wantErr: "GOCDNEXT_APPROVER_GROUPS_TIMEOUT"},
		{name: "timeout above range aborts", url: "https://zen.example.com/groups", tok: token, timeout: "1m",
			wantErr: "GOCDNEXT_APPROVER_GROUPS_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOCDNEXT_DATABASE_URL", "postgres://x")
			t.Setenv("GOCDNEXT_APPROVER_GROUPS_URL", tt.url)
			t.Setenv("GOCDNEXT_APPROVER_GROUPS_TOKEN", tt.tok)
			t.Setenv("GOCDNEXT_APPROVER_GROUPS_TIMEOUT", tt.timeout)

			c, err := config.Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want mention of %s", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), token) {
					t.Errorf("error %q leaks the token", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			g := c.ApproverGroups
			if g.URL != tt.wantURL || g.Timeout != tt.wantTimeout {
				t.Errorf("ApproverGroups = {URL:%q Timeout:%v}, want {%q %v}", g.URL, g.Timeout, tt.wantURL, tt.wantTimeout)
			}
			if tt.wantURL != "" && g.Token != token {
				t.Errorf("Token not loaded")
			}
		})
	}
}
