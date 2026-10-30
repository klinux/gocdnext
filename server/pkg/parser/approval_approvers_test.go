package parser

import (
	"strings"
	"testing"
)

func approversYAML(list string) string {
	return `name: ci
stages: [deploy]
jobs:
  gate:
    stage: deploy
    approval:
      approvers: ` + list + `
`
}

func TestParseApprovers_SubjectRefs(t *testing.T) {
	tests := []struct {
		name    string
		list    string
		want    []string
		wantErr string
	}{
		{"plain names and emails pass", `[alice, "bob@example.com"]`, []string{"alice", "bob@example.com"}, ""},
		{"subject ref passes", `["sub:oidc:3f2a-91", alice]`, []string{"sub:oidc:3f2a-91", "alice"}, ""},
		{"subject ref missing subject", `["sub:oidc:"]`, nil, "missing the subject"},
		{"subject ref missing provider", `["sub::3f2a"]`, nil, "missing the provider"},
		{"subject ref with whitespace", `["sub:oidc:3f2a 91"]`, nil, "whitespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Parse(strings.NewReader(approversYAML(tt.list)), "proj", "ci")
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), `"gate"`) {
					t.Fatalf("error %q should cite the job and mention %q", err, tt.wantErr)
				}
				// The entry may be a real IdP subject — the error cites the
				// index, never the value.
				if strings.Contains(err.Error(), "3f2a") {
					t.Fatalf("error %q leaks the approver entry", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := p.Jobs[0].Approval.Approvers
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("approvers = %v, want %v", got, tt.want)
			}
		})
	}
}
