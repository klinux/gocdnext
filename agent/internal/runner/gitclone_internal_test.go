package runner

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactURLCredential(t *testing.T) {
	const secret = "SUPERSECRET"
	tests := []struct {
		name string
		in   string
		want string // exact when set
		// when want is empty we only assert secret-absent + host-present
		host string
	}{
		{
			name: "bare https userinfo",
			in:   "https://x-access-token:SUPERSECRET@host.example/repo.git",
			want: "https://***@host.example/repo.git",
		},
		{
			name: "git fatal mid-sentence quoted",
			in:   "fatal: unable to access 'https://x-access-token:SUPERSECRET@host.example/repo.git/': 403",
			host: "host.example",
		},
		{
			name: "two urls redirected",
			in:   "warning: redirected from https://u:SUPERSECRET@a.example to https://v:SUPERSECRET@b.example",
			host: "a.example",
		},
		{
			name: "ssh scheme userinfo",
			in:   "ssh://git:SUPERSECRET@host.example:22/repo",
			want: "ssh://***@host.example:22/repo",
		},
		{
			name: "percent-encoded credential",
			in:   "https://user:p%40ssSUPERSECRET@host.example/x",
			want: "https://***@host.example/x",
		},
		{
			name: "bearer as sole userinfo",
			in:   "https://SUPERSECRET@host.example/x",
			want: "https://***@host.example/x",
		},
		{
			name: "no userinfo untouched",
			in:   "https://host.example/org/repo.git",
			want: "https://host.example/org/repo.git",
		},
		{
			name: "scp-like git@ is not a credential",
			in:   "git@github.com:org/repo.git",
			want: "git@github.com:org/repo.git",
		},
		{
			name: "query string at-sign not userinfo",
			in:   "https://host.example/path?email=user@corp.com",
			want: "https://host.example/path?email=user@corp.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactURLCredential(tt.in)
			if strings.Contains(got, secret) {
				t.Fatalf("secret leaked: %q", got)
			}
			if tt.want != "" && got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			if tt.host != "" && !strings.Contains(got, tt.host) {
				t.Fatalf("host %q missing from %q", tt.host, got)
			}
		})
	}
}

func TestCloneClassifier(t *testing.T) {
	tests := []struct {
		name         string
		lines        []string
		wantFallback bool
	}{
		{
			name:         "filter unsupported triggers fallback",
			lines:        []string{"fatal: invalid filter-spec 'blob:none'", "error: filter unsupported by server"},
			wantFallback: true,
		},
		{
			name:         "unadvertised promisor object triggers fallback",
			lines:        []string{"fatal: request for unadvertised object"},
			wantFallback: true,
		},
		{
			name:         "missing plus promisor triggers fallback",
			lines:        []string{"error: missing object required for partial clone (promisor)"},
			wantFallback: true,
		},
		{
			name:         "normal missing revision does NOT fall back",
			lines:        []string{"error: pathspec 'deadbeef' did not match any file(s) known to git", "fatal: reference is not a tree"},
			wantFallback: false,
		},
		{
			name:         "auth failure does NOT fall back",
			lines:        []string{"fatal: Authentication failed for 'https://***@host/x'"},
			wantFallback: false,
		},
		{
			name:         "network failure does NOT fall back",
			lines:        []string{"fatal: unable to access: Could not resolve host: host.example"},
			wantFallback: false,
		},
		{
			// The decisive case: a promisor signal AND an auth line — the veto wins.
			name:         "promisor co-occurring with auth is vetoed",
			lines:        []string{"error: missing object required for partial clone (promisor)", "fatal: Authentication failed"},
			wantFallback: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c cloneClassifier
			for _, l := range tt.lines {
				c.observe(l)
			}
			if got := c.shouldFallback(); got != tt.wantFallback {
				t.Fatalf("shouldFallback = %v, want %v (%+v)", got, tt.wantFallback, c)
			}
		})
	}
}

func TestValidateTargetWithinBase(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{"child ok", filepath.Join(base, "fixture"), false},
		{"nested child ok", filepath.Join(base, "a", "b"), false},
		{"base itself refused", base, true},
		{"parent escape refused", filepath.Join(base, ".."), true},
		{"sibling with shared prefix refused", base + "2", true},
		{"absolute elsewhere refused", "/etc", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTargetWithinBase(base, tt.target)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
