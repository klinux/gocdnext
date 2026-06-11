package oidcissuer

import (
	"testing"
	"time"
)

// TestSubject_PerCause — the sub grammar is the policy-matching
// surface cloud operators pin IAM conditions to. Each shape is a
// CONTRACT: changing any of these strings breaks every WIF policy
// in the wild.
func TestSubject_PerCause(t *testing.T) {
	tests := []struct {
		name string
		jc   JobClaims
		want string
	}{
		{
			name: "branch run",
			jc: JobClaims{
				ProjectSlug: "shop", Pipeline: "ci",
				RefType: "branch", Ref: "main", Cause: "webhook",
			},
			want: "project:shop:pipeline:ci:ref_type:branch:ref:main",
		},
		{
			name: "tag run",
			jc: JobClaims{
				ProjectSlug: "shop", Pipeline: "release",
				RefType: "tag", Ref: "v1.2.3", Cause: "tag",
			},
			want: "project:shop:pipeline:release:ref_type:tag:ref:v1.2.3",
		},
		{
			// PR head refs are attacker-controlled: a PR from branch
			// "main" must NEVER satisfy a ref:main cloud policy. The
			// PR sub therefore carries NO ref segment at all.
			name: "pull request run",
			jc: JobClaims{
				ProjectSlug: "shop", Pipeline: "ci",
				RefType: "branch", Ref: "main", Cause: "pull_request",
			},
			want: "project:shop:pipeline:ci:pull_request",
		},
		{
			name: "manual run without material",
			jc: JobClaims{
				ProjectSlug: "shop", Pipeline: "ops",
				RefType: "", Ref: "", Cause: "manual",
			},
			want: "project:shop:pipeline:ops:ref_type:none:ref:none",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.jc.Subject(); got != tt.want {
				t.Errorf("Subject() = %q\nwant        %q", got, tt.want)
			}
		})
	}
}

// TestSubject_EscapesGrammarCollisions — a pipeline name (or slug)
// carrying ':' would let an operator craft a sub that impersonates
// another segment shape. Such characters are percent-encoded so the
// grammar stays unambiguous regardless of input.
func TestSubject_EscapesGrammarCollisions(t *testing.T) {
	jc := JobClaims{
		ProjectSlug: "shop", Pipeline: "ci:prod", // ':' must not survive raw
		RefType: "branch", Ref: "main", Cause: "webhook",
	}
	got := jc.Subject()
	want := "project:shop:pipeline:ci%3Aprod:ref_type:branch:ref:main"
	if got != want {
		t.Errorf("Subject() = %q, want %q", got, want)
	}
}

// TestPayload_Schema — table over the payload builder: registered
// claims computed from the injected clock, custom claims all
// string-typed, omission rules honoured, aud scalar-vs-array.
func TestPayload_Schema(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	jc := JobClaims{
		ProjectSlug: "shop", ProjectID: "pid", Pipeline: "ci", PipelineID: "plid",
		Job: "deploy", MatrixKey: "shard=a", RunID: "rid", RunCounter: "42",
		Ref: "main", RefType: "branch", SHA: "abc123", Cause: "webhook",
	}

	p := buildPayload("https://ci.example.com", jc, []string{"aud-1"}, now, time.Hour, "jti-1")

	if p["iss"] != "https://ci.example.com" {
		t.Errorf("iss = %v", p["iss"])
	}
	if p["sub"] != jc.Subject() {
		t.Errorf("sub = %v", p["sub"])
	}
	// Single audience → plain string (GitLab/GHA convention; some
	// verifiers reject single-element arrays).
	if p["aud"] != "aud-1" {
		t.Errorf("aud = %#v, want scalar string", p["aud"])
	}
	if p["iat"] != now.Unix() {
		t.Errorf("iat = %v, want %v", p["iat"], now.Unix())
	}
	if p["nbf"] != now.Add(-60*time.Second).Unix() {
		t.Errorf("nbf = %v, want iat-60s", p["nbf"])
	}
	if p["exp"] != now.Add(time.Hour).Unix() {
		t.Errorf("exp = %v, want iat+ttl", p["exp"])
	}
	if p["jti"] != "jti-1" {
		t.Errorf("jti = %v", p["jti"])
	}

	// Custom claims: all present, all strings.
	for _, k := range []string{"project_slug", "project_id", "pipeline", "pipeline_id", "job", "matrix_key", "run_id", "run_counter", "ref", "ref_type", "sha", "cause"} {
		v, present := p[k]
		if !present {
			t.Errorf("claim %q missing", k)
			continue
		}
		if _, isStr := v.(string); !isStr {
			t.Errorf("claim %q is %T, want string (cloud attribute-mapping ergonomics)", k, v)
		}
	}

	// Multi-audience → array.
	p2 := buildPayload("https://ci.example.com", jc, []string{"a", "b"}, now, time.Hour, "jti-2")
	arr, ok := p2["aud"].([]string)
	if !ok || len(arr) != 2 {
		t.Errorf("multi aud = %#v, want []string of 2", p2["aud"])
	}
}

// TestPayload_OmissionRules — matrix_key/sha/ref/ref_type/pr_number
// are omitted (absent, not empty-string) when not applicable, so
// cloud attribute mappings don't match on "".
func TestPayload_OmissionRules(t *testing.T) {
	now := time.Now()

	// Manual run without material, no matrix.
	bare := buildPayload("https://x", JobClaims{
		ProjectSlug: "s", ProjectID: "p", Pipeline: "pl", PipelineID: "pli",
		Job: "j", RunID: "r", RunCounter: "1", Cause: "manual",
	}, []string{"a"}, now, time.Minute, "j1")
	for _, k := range []string{"matrix_key", "ref", "sha", "pr_number"} {
		if _, present := bare[k]; present {
			t.Errorf("claim %q present on bare run, want omitted", k)
		}
	}
	// ref_type IS always present — "none" is an explicit, matchable
	// value (policies can require ref_type:branch and naturally
	// exclude manual runs).
	if bare["ref_type"] != "none" {
		t.Errorf("ref_type = %v, want \"none\"", bare["ref_type"])
	}

	// PR run carries pr_number.
	pr := buildPayload("https://x", JobClaims{
		ProjectSlug: "s", ProjectID: "p", Pipeline: "pl", PipelineID: "pli",
		Job: "j", RunID: "r", RunCounter: "1", Cause: "pull_request",
		Ref: "feat", RefType: "branch", PRNumber: "7",
	}, []string{"a"}, now, time.Minute, "j2")
	if pr["pr_number"] != "7" {
		t.Errorf("pr_number = %v, want \"7\"", pr["pr_number"])
	}
}
