package parser

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// TestParse_IDTokens_AcceptsDeclaration — happy path: scalar and
// list aud forms, multiple tokens per job, all landing on
// domain.Job.IDTokens.
func TestParse_IDTokens_AcceptsDeclaration(t *testing.T) {
	const y = `
name: deploy
stages: [ship]
materials:
  - manual: true
jobs:
  ship:
    stage: ship
    image: alpine
    script: ["./deploy.sh"]
    id_tokens:
      GCP_ID_TOKEN:
        aud: https://iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/x
      VAULT_JWT:
        aud: [https://vault.example.com, https://vault-dr.example.com]
`
	p, err := ParseNamed(strings.NewReader(y), "p", "deploy")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	job := findJob(t, p, "ship")
	if len(job.IDTokens) != 2 {
		t.Fatalf("id_tokens len = %d, want 2 (%v)", len(job.IDTokens), job.IDTokens)
	}
	gcp := job.IDTokens["GCP_ID_TOKEN"]
	if len(gcp.Aud) != 1 || !strings.HasPrefix(gcp.Aud[0], "https://iam.googleapis.com/") {
		t.Errorf("GCP_ID_TOKEN aud = %v", gcp.Aud)
	}
	vault := job.IDTokens["VAULT_JWT"]
	if len(vault.Aud) != 2 {
		t.Errorf("VAULT_JWT aud = %v, want 2 entries", vault.Aud)
	}
}

// TestParse_IDTokens_Rejections — every malformed shape fails at
// parse time with a message naming the problem. Table mirrors the
// outputs/masked rejection conventions.
func TestParse_IDTokens_Rejections(t *testing.T) {
	const tmpl = `
name: deploy
stages: [ship]
materials:
  - manual: true
jobs:
  ship:
    stage: ship
    image: alpine
    script: ["true"]
%s
`
	tests := []struct {
		name    string
		block   string
		wantSub string
	}{
		{
			name: "missing aud",
			block: `    id_tokens:
      MY_TOKEN: {}`,
			wantSub: "aud",
		},
		{
			name: "empty aud scalar",
			block: `    id_tokens:
      MY_TOKEN:
        aud: ""`,
			wantSub: "aud",
		},
		{
			name: "empty aud list",
			block: `    id_tokens:
      MY_TOKEN:
        aud: []`,
			wantSub: "aud",
		},
		{
			name: "blank entry inside aud list",
			block: `    id_tokens:
      MY_TOKEN:
        aud: ["https://ok", ""]`,
			wantSub: "aud",
		},
		{
			name: "audience typo",
			block: `    id_tokens:
      MY_TOKEN:
        audience: https://x`,
			wantSub: "unknown key",
		},
		{
			name: "scalar entry instead of mapping",
			block: `    id_tokens:
      MY_TOKEN: https://x`,
			wantSub: "must be an object",
		},
		{
			name: "invalid env var name",
			block: `    id_tokens:
      my-token:
        aud: https://x`,
			wantSub: "env var name",
		},
		{
			name: "reserved CI_ prefix",
			block: `    id_tokens:
      CI_SHADOW:
        aud: https://x`,
			wantSub: "reserved",
		},
		{
			name: "reserved GOCDNEXT_ prefix",
			block: `    id_tokens:
      GOCDNEXT_X:
        aud: https://x`,
			wantSub: "reserved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := strings.Replace(tmpl, "%s", tt.block, 1)
			_, err := ParseNamed(strings.NewReader(y), "p", "deploy")
			if err == nil {
				t.Fatalf("expected parse error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestParse_IDTokens_CollisionWithVariablesAndSecrets — an id_token
// env name colliding with the job's variables: or secrets: would be
// resolved by silent map-layering order at dispatch; fail loud at
// parse instead.
func TestParse_IDTokens_CollisionWithVariablesAndSecrets(t *testing.T) {
	const varsCollision = `
name: deploy
stages: [ship]
materials:
  - manual: true
jobs:
  ship:
    stage: ship
    image: alpine
    script: ["true"]
    variables:
      MY_TOKEN: oops
    id_tokens:
      MY_TOKEN:
        aud: https://x
`
	if _, err := ParseNamed(strings.NewReader(varsCollision), "p", "deploy"); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Errorf("variables collision: err = %v, want mention of collides", err)
	}

	const secretsCollision = `
name: deploy
stages: [ship]
materials:
  - manual: true
jobs:
  ship:
    stage: ship
    image: alpine
    script: ["true"]
    secrets: [MY_TOKEN]
    id_tokens:
      MY_TOKEN:
        aud: https://x
`
	if _, err := ParseNamed(strings.NewReader(secretsCollision), "p", "deploy"); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Errorf("secrets collision: err = %v, want mention of collides", err)
	}

	// Pipeline-LEVEL variables land in the same env map at dispatch
	// (before job vars, before the token) — a collision here would
	// be silently resolved by layering order, so it must fail at
	// apply just like the job-level one. Review round 1 finding.
	const pipelineVarsCollision = `
name: deploy
stages: [ship]
materials:
  - manual: true
variables:
  MY_TOKEN: oops-pipeline-level
jobs:
  ship:
    stage: ship
    image: alpine
    script: ["true"]
    id_tokens:
      MY_TOKEN:
        aud: https://x
`
	if _, err := ParseNamed(strings.NewReader(pipelineVarsCollision), "p", "deploy"); err == nil || !strings.Contains(err.Error(), "pipeline-level") {
		t.Errorf("pipeline vars collision: err = %v, want mention of pipeline-level", err)
	}
}

// TestParse_IDTokens_JSONRoundTrip — the pipeline definition is
// persisted as JSONB and re-decoded at dispatch; IDTokens must
// survive the trip intact.
func TestParse_IDTokens_JSONRoundTrip(t *testing.T) {
	const y = `
name: deploy
stages: [ship]
materials:
  - manual: true
jobs:
  ship:
    stage: ship
    image: alpine
    script: ["true"]
    id_tokens:
      GCP_TOKEN:
        aud: [https://a, https://b]
`
	p, err := ParseNamed(strings.NewReader(y), "p", "deploy")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	blob, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back domain.Pipeline
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var ship *domain.Job
	for i := range back.Jobs {
		if back.Jobs[i].Name == "ship" {
			ship = &back.Jobs[i]
		}
	}
	if ship == nil {
		t.Fatalf("ship job missing after round-trip")
	}
	if len(ship.IDTokens["GCP_TOKEN"].Aud) != 2 {
		t.Errorf("IDTokens after round-trip = %+v", ship.IDTokens)
	}
}

// TestParse_IDTokens_RejectedOnApprovalGate — approval gates are a
// pure state-machine construct and never dispatch a container, so
// a declared token would never be minted (silently) AND the
// lingering "IDTokens" key in the JSONB would defeat the
// scheduler's fast-path gate for the whole run. id_tokens is an
// execution knob; the approval block must reject it like
// script/image/cache. Review round 5.
func TestParse_IDTokens_RejectedOnApprovalGate(t *testing.T) {
	const y = `
name: deploy
stages: [gate]
materials:
  - manual: true
jobs:
  approve:
    stage: gate
    approval:
      approvers: [admin]
    id_tokens:
      GCP_TOKEN:
        aud: https://x
`
	_, err := ParseNamed(strings.NewReader(y), "p", "deploy")
	if err == nil {
		t.Fatalf("expected parse error for approval + id_tokens")
	}
	if !strings.Contains(err.Error(), "approval gate") || !strings.Contains(err.Error(), "id_tokens") {
		t.Errorf("err = %v, want approval-gate rejection naming id_tokens", err)
	}
}

// TestParse_NoIDTokens_OmittedFromJSON — a pipeline WITHOUT
// id_tokens must marshal without the "IDTokens" key at all. The
// scheduler's dispatch fast path gates on
// bytes.Contains(definition, `"IDTokens"`) before paying any JSON
// decode; a `"IDTokens":null` on every job would defeat the gate
// for the entire install. Review round 2.
func TestParse_NoIDTokens_OmittedFromJSON(t *testing.T) {
	const y = `
name: plain
stages: [s]
materials:
  - manual: true
jobs:
  j:
    stage: s
    image: alpine
    script: ["true"]
`
	p, err := ParseNamed(strings.NewReader(y), "p", "plain")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	blob, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), `"IDTokens"`) {
		t.Errorf("definition without id_tokens carries the IDTokens key — dispatch fast path defeated:\n%s", blob)
	}
}

// TestParse_PipelineNameRejectsColon — ':' in a pipeline name would
// let an operator craft sub-claim segments that impersonate the
// grammar (project:x:pipeline:ci:prod:ref...). The sub builder also
// percent-encodes as defence in depth, but the front door rejects.
func TestParse_PipelineNameRejectsColon(t *testing.T) {
	const y = `
name: "ci:prod"
stages: [s]
materials:
  - manual: true
jobs:
  j:
    stage: s
    image: alpine
    script: ["true"]
`
	if _, err := ParseNamed(strings.NewReader(y), "p", "fallback"); err == nil || !strings.Contains(err.Error(), ":") {
		t.Errorf("err = %v, want rejection of ':' in pipeline name", err)
	}
}
