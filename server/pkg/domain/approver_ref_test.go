package domain

import (
	"strings"
	"testing"
)

func TestApproverSubjectRef(t *testing.T) {
	tests := []struct {
		name, provider, subject, want string
	}{
		{"oidc", "oidc", "3f2a-91", "sub:oidc:3f2a-91"},
		{"github numeric id", "github", "12345", "sub:github:12345"},
		{"urn subject keeps its colons", "keycloak", "urn:x:1", "sub:keycloak:urn:x:1"},
		{"missing provider", "", "3f2a", ""},
		{"missing subject", "oidc", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ApproverSubjectRef(tt.provider, tt.subject); got != tt.want {
				t.Errorf("ApproverSubjectRef(%q, %q) = %q, want %q", tt.provider, tt.subject, got, tt.want)
			}
		})
	}
}

func TestValidateApproverRef(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"plain email", "alice@example.com", false},
		{"plain display name with spaces", "Alice Smith", false},
		{"subject ref", "sub:oidc:3f2a-91", false},
		{"subject ref with urn subject", "sub:keycloak:urn:x:1", false},
		{"subject ref without provider", "sub::3f2a", true},
		{"subject ref without subject", "sub:oidc:", true},
		{"subject ref without separator", "sub:oidc", true},
		{"bare prefix", "sub:", true},
		{"whitespace in subject", "sub:oidc:3f2a 91", true},
		{"control char in provider", "sub:oi\x00dc:3f2a", true},
		{"oversized subject", "sub:oidc:" + strings.Repeat("a", maxApproverSubjectLen+1), true},
		{"subject at the limit", "sub:oidc:" + strings.Repeat("a", maxApproverSubjectLen), false},
		{"oversized provider", "sub:" + strings.Repeat("p", maxApproverProviderLen+1) + ":x", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateApproverRef(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateApproverRef(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestValidateApproverRef_ErrorNeverEchoesTheValue(t *testing.T) {
	// The ref may be a real IdP subject; the error names the problem, not the value.
	err := ValidateApproverRef("sub:oidc:secret-ish value")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "secret-ish") {
		t.Errorf("error leaks the subject: %v", err)
	}
}
