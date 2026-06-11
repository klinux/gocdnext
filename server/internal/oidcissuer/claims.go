package oidcissuer

import (
	"strings"
	"time"
)

// JobClaims is everything the scheduler knows about a job at
// dispatch time that ends up inside the token. All fields are
// strings on purpose — GCP/AWS/Vault attribute mappings consume
// string claims far more ergonomically than numbers, and GitLab/GHA
// follow the same convention.
type JobClaims struct {
	ProjectSlug string
	ProjectID   string
	Pipeline    string
	PipelineID  string
	Job         string
	MatrixKey   string // empty when the job isn't matrix-expanded
	RunID       string
	RunCounter  string
	Ref         string // branch or tag name; empty when no material
	RefType     string // "branch" | "tag" | "" (normalized to "none")
	SHA         string // empty when no material
	Cause       string // webhook|pull_request|manual|upstream|schedule|poll|tag
	PRNumber    string // only on pull_request cause
}

// Subject builds the `sub` claim — THE policy-matching surface cloud
// operators pin IAM conditions to. The grammar is a contract; every
// shape here is asserted by tests because changing one breaks every
// WIF policy in the wild.
//
//	branch:  project:{slug}:pipeline:{name}:ref_type:branch:ref:{branch}
//	tag:     project:{slug}:pipeline:{name}:ref_type:tag:ref:{tag}
//	PR:      project:{slug}:pipeline:{name}:pull_request
//	no ref:  project:{slug}:pipeline:{name}:ref_type:none:ref:none
//
// PR runs carry NO ref segment: the head ref name is attacker-
// controlled (a PR from a branch named "main" must never satisfy a
// `...ref:main` policy), so PR exclusion is structural, not
// something operators must remember. Finer policy goes through the
// `cause` / `pr_number` custom claims.
func (c JobClaims) Subject() string {
	base := "project:" + escapeSub(c.ProjectSlug) + ":pipeline:" + escapeSub(c.Pipeline)
	if c.Cause == "pull_request" {
		return base + ":pull_request"
	}
	refType, ref := c.RefType, c.Ref
	if refType == "" || ref == "" {
		refType, ref = "none", "none"
	}
	return base + ":ref_type:" + refType + ":ref:" + escapeSub(ref)
}

// escapeSub percent-encodes ':' (and '%' so the encoding stays
// reversible) inside a sub segment. Without this, a pipeline named
// "ci:prod" could impersonate grammar segments and trick a sloppy
// glob policy. Slugs are validated at apply time but pipeline names
// come straight from YAML — defence in depth over trusting upstream
// validation.
func escapeSub(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	return strings.ReplaceAll(s, ":", "%3A")
}

// buildPayload assembles the full JWT payload. Single audience is
// emitted as a plain string (some verifiers reject single-element
// arrays); multiple audiences as an array. Optional claims are
// OMITTED (not empty strings) when absent so cloud attribute
// mappings never match on "".
func buildPayload(issuer string, jc JobClaims, aud []string, now time.Time, ttl time.Duration, jti string) map[string]any {
	p := map[string]any{
		"iss": issuer,
		"sub": jc.Subject(),
		"iat": now.Unix(),
		// 60s backdate absorbs server↔verifier clock skew — same
		// constant the GitHub App JWT path uses.
		"nbf": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(ttl).Unix(),
		"jti": jti,

		"project_slug": jc.ProjectSlug,
		"project_id":   jc.ProjectID,
		"pipeline":     jc.Pipeline,
		"pipeline_id":  jc.PipelineID,
		"job":          jc.Job,
		"run_id":       jc.RunID,
		"run_counter":  jc.RunCounter,
		"cause":        jc.Cause,
	}
	if len(aud) == 1 {
		p["aud"] = aud[0]
	} else {
		p["aud"] = aud
	}
	// ref_type is ALWAYS present — "none" is an explicit, matchable
	// value so `ref_type:branch` policies naturally exclude manual
	// runs instead of relying on a missing-claim behaviour that
	// differs per cloud.
	if jc.RefType == "" || jc.Ref == "" {
		p["ref_type"] = "none"
	} else {
		p["ref_type"] = jc.RefType
		p["ref"] = jc.Ref
	}
	if jc.MatrixKey != "" {
		p["matrix_key"] = jc.MatrixKey
	}
	if jc.SHA != "" {
		p["sha"] = jc.SHA
	}
	if jc.PRNumber != "" {
		p["pr_number"] = jc.PRNumber
	}
	return p
}
