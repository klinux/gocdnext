package external

import (
	"strings"
	"testing"
)

func TestGCPResourceName(t *testing.T) {
	tests := []struct {
		name    string
		project string
		path    string
		key     string
		want    string
		wantErr string // substring; "" = expect success
	}{
		{
			name:    "bare id, no version",
			project: "acme",
			path:    "gh-token",
			want:    "projects/acme/secrets/gh-token/versions/latest",
		},
		{
			name:    "bare id, explicit version",
			project: "acme",
			path:    "gh-token",
			key:     "3",
			want:    "projects/acme/secrets/gh-token/versions/3",
		},
		{
			name:    "full resource name in the configured project, no version",
			project: "acme",
			path:    "projects/acme/secrets/gh-token",
			want:    "projects/acme/secrets/gh-token/versions/latest",
		},
		{
			name:    "full resource name with version baked in is stripped, key wins",
			project: "acme",
			path:    "projects/acme/secrets/gh-token/versions/9",
			key:     "5",
			want:    "projects/acme/secrets/gh-token/versions/5",
		},
		{
			name:    "full resource name with trailing version, no key → latest",
			project: "acme",
			path:    "projects/acme/secrets/gh-token/versions/9",
			want:    "projects/acme/secrets/gh-token/versions/latest",
		},
		{
			name:    "cross-project full resource name is rejected (tenancy guard)",
			project: "acme",
			path:    "projects/other/secrets/gh-token",
			wantErr: "configured for",
		},
		{
			name:    "malformed resource name is rejected",
			project: "acme",
			path:    "projects/acme/gh-token",
			wantErr: "malformed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gcpResourceName(tt.project, tt.path, tt.key)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("gcpResourceName(%q,%q,%q) err = %v, want substring %q", tt.project, tt.path, tt.key, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tt.want {
				t.Errorf("gcpResourceName(%q,%q,%q) = %q, want %q", tt.project, tt.path, tt.key, got, tt.want)
			}
		})
	}
}
