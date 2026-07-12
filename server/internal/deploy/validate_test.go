package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidateTargetFields(t *testing.T) {
	tests := []struct {
		name                                     string
		provider, cluster, application, syncMode string
		wantErr                                  string // substring; "" = no error
	}{
		{"ok", "argocd", "prod", "checkout", "trigger", ""},
		{"ok observe", "argocd", "prod", "checkout", "observe", ""},
		{"bad provider", "flux", "prod", "checkout", "trigger", "provider"},
		{"bad sync_mode", "argocd", "prod", "checkout", "auto", "sync_mode"},
		{"empty cluster", "argocd", "  ", "checkout", "trigger", "cluster is required"},
		{"empty application", "argocd", "prod", "", "trigger", "application is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTargetFields(tt.provider, tt.cluster, tt.application, tt.syncMode)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeNamespace(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "argocd"},
		{"   ", "argocd"},
		{"custom-ns", "custom-ns"},
		{"  padded  ", "padded"},
	}
	for _, tt := range tests {
		if got := NormalizeNamespace(tt.in); got != tt.want {
			t.Errorf("NormalizeNamespace(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestApplicationIsMultiSource(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"single source", `{"spec":{"source":{"repoURL":"x"}}}`, false},
		{"no spec", `{}`, false},
		{"multi source", `{"spec":{"sources":[{"repoURL":"a"},{"repoURL":"b"}]}}`, true},
		{"empty sources list", `{"spec":{"sources":[]}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applicationIsMultiSource([]byte(tt.raw))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("applicationIsMultiSource(%s) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
	if _, err := applicationIsMultiSource([]byte(`{bad`)); err == nil {
		t.Error("expected an error on malformed JSON")
	}
}

func TestValidateSingleSource(t *testing.T) {
	target := DeploymentTarget{Application: "checkout", Namespace: "argocd"}

	t.Run("single-source passes", func(t *testing.T) {
		p := newArgoProviderWith(fakeFetcher{raw: []byte(`{"spec":{"source":{"repoURL":"x"}},"status":{}}`)})
		if err := p.ValidateSingleSource(context.Background(), target); err != nil {
			t.Fatalf("ValidateSingleSource: %v", err)
		}
	})

	t.Run("multi-source is rejected", func(t *testing.T) {
		p := newArgoProviderWith(fakeFetcher{raw: []byte(`{"spec":{"sources":[{"repoURL":"a"},{"repoURL":"b"}]}}`)})
		err := p.ValidateSingleSource(context.Background(), target)
		if err == nil || !strings.Contains(err.Error(), "multi-source") {
			t.Fatalf("err = %v, want a multi-source rejection", err)
		}
	})

	t.Run("a fetch error (missing/unreachable/unauthorized) surfaces", func(t *testing.T) {
		sentinel := errors.New("project not authorized for cluster")
		p := newArgoProviderWith(fakeFetcher{err: sentinel})
		if err := p.ValidateSingleSource(context.Background(), target); !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want it to wrap %v", err, sentinel)
		}
	})
}
