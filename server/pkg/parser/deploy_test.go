package parser

import (
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func findJobByName(t *testing.T, p *domain.Pipeline, name string) domain.Job {
	t.Helper()
	for _, j := range p.Jobs {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("job %q not found in pipeline", name)
	return domain.Job{}
}

// #39 deployment primitive — the `deploy:` block is a tracking MARKER
// on an executable job (the plugin/script still does the deploy). The
// parser records environment + version into the domain Job; the
// scheduler resolves version and writes a deployment_revision.

func TestParse_Deploy_AcceptsMarkerOnExecutableJob(t *testing.T) {
	y := `
stages: [promote]
jobs:
  sync-prod:
    stage: promote
    uses: ghcr.io/klinux/gocdnext-plugin-argocd@v1
    with:
      command: app sync zen-prod
    deploy:
      environment: production
      version: ${{ needs.build.outputs.image-tag }}
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	job := findJobByName(t, p, "sync-prod")
	if job.Deploy == nil {
		t.Fatal("Deploy spec not carried to domain.Job")
	}
	if job.Deploy.Environment != "production" {
		t.Errorf("environment = %q, want production", job.Deploy.Environment)
	}
	if job.Deploy.Version != "${{ needs.build.outputs.image-tag }}" {
		t.Errorf("version = %q, want the raw ref (resolved later at dispatch)", job.Deploy.Version)
	}
}

func TestParse_Deploy_VersionOptional(t *testing.T) {
	// Omitted version is legal — the scheduler defaults it to the
	// commit short sha. Parser keeps it empty.
	y := `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["kubectl apply -f ."]
    deploy:
      environment: staging
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	job := findJobByName(t, p, "ship")
	if job.Deploy == nil || job.Deploy.Environment != "staging" {
		t.Fatalf("deploy spec = %+v", job.Deploy)
	}
	if job.Deploy.Version != "" {
		t.Errorf("version should stay empty when omitted, got %q", job.Deploy.Version)
	}
}

// revision is the correlation anchor and is INDEPENDENT of the display version — the
// parser keeps both verbatim and never derives one from the other.
func TestParse_Deploy_AcceptsRevision(t *testing.T) {
	y := `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["kubectl apply -f ."]
    deploy:
      environment: production
      version: 1.27.abc1234
      revision: ${{ CI_COMMIT_SHA }}
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	job := findJobByName(t, p, "ship")
	if job.Deploy == nil {
		t.Fatal("deploy spec missing")
	}
	if job.Deploy.Version != "1.27.abc1234" {
		t.Errorf("version = %q, want the label untouched", job.Deploy.Version)
	}
	if job.Deploy.Revision != "${{ CI_COMMIT_SHA }}" {
		t.Errorf("revision = %q, want the raw ref preserved for dispatch-time resolution", job.Deploy.Revision)
	}
}

func TestParse_Deploy_RevisionOptional(t *testing.T) {
	y := `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      environment: staging
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if job := findJobByName(t, p, "ship"); job.Deploy.Revision != "" {
		t.Errorf("revision should stay empty when omitted, got %q", job.Deploy.Revision)
	}
}

func TestParse_Deploy_AcceptsNeedsAndCIRefsInVersion(t *testing.T) {
	// needs.outputs and CI_* refs are the allowed version namespaces;
	// a mix with literals + shell-style ${CI_*} must parse clean.
	y := `
stages: [build, deploy]
jobs:
  build:
    stage: build
    script: ["make build"]
    outputs:
      sha: SHA
  ship:
    stage: deploy
    needs: [build]
    script: ["true"]
    deploy:
      environment: production
      version: "1.${{ CI_RUN_COUNTER }}.${{ needs.build.outputs.sha }}-${CI_COMMIT_SHORT_SHA}"
`
	if _, err := ParseNamed(strings.NewReader(y), "p", "release"); err != nil {
		t.Fatalf("parse: %v", err)
	}
}

func TestParse_Deploy_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "missing environment",
			yaml: `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      version: abc
`,
			wantErr: "environment",
		},
		{
			name: "environment with forbidden chars",
			yaml: `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      environment: "prod; rm -rf"
`,
			wantErr: "environment",
		},
		{
			name: "deploy on non-executable job",
			yaml: `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    deploy:
      environment: production
`,
			wantErr: "executable",
		},
		{
			name: "deploy together with approval",
			yaml: `
stages: [promote]
jobs:
  gate:
    stage: promote
    approval:
      approvers: [alice]
    deploy:
      environment: production
`,
			wantErr: "approval",
		},
		{
			name: "unknown key (typo env: for environment:)",
			yaml: `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      env: production
`,
			wantErr: "environment",
		},
		{
			name: "version with disallowed variable ref",
			yaml: `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      environment: production
      version: "${{ MY_VAR }}"
`,
			wantErr: "not allowed",
		},
		{
			name: "version with disallowed secret ref",
			yaml: `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      environment: production
      version: "${{ SECRET_TOKEN }}"
`,
			wantErr: "not allowed",
		},
		{
			// revision is persisted as expected_revision and shown in the UI, so it
			// inherits the SAME non-secret allow-list as version.
			name: "revision with disallowed secret ref",
			yaml: `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      environment: production
      revision: "${{ SECRET_TOKEN }}"
`,
			wantErr: "not allowed",
		},
		{
			name: "revision with disallowed variable ref",
			yaml: `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      environment: production
      revision: "${{ MY_VAR }}"
`,
			wantErr: "not allowed",
		},
		{
			// The hand-written UnmarshalYAML must keep rejecting unknown keys now that
			// it has a third accepted one — a typo must not be silently dropped.
			name: "unknown key next to revision",
			yaml: `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      environment: production
      revison: abc
`,
			wantErr: "unknown key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseNamed(strings.NewReader(tt.yaml), "p", "release")
			if err == nil {
				t.Fatalf("expected rejection for %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
			}
		})
	}
}

func TestParse_DeployTarget_Accepted(t *testing.T) {
	y := `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      environment: production
      target:
        cluster: prod-hub
        application: shop-prod
        namespace: argocd
        sync_mode: trigger
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tgt := findJobByName(t, p, "ship").Deploy.Target
	if tgt == nil {
		t.Fatal("target missing")
	}
	if tgt.Cluster != "prod-hub" || tgt.Application != "shop-prod" ||
		tgt.Namespace != "argocd" || tgt.SyncMode != "trigger" {
		t.Fatalf("target = %+v", tgt)
	}
}

// namespace is intentionally NOT defaulted at parse: the write boundary owns that
// default, so duplicating it here would give the value two owners that can drift.
func TestParse_DeployTarget_NamespaceLeftToTheWriteBoundary(t *testing.T) {
	y := `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      environment: production
      target:
        cluster: prod-hub
        application: shop-prod
        sync_mode: observe
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ns := findJobByName(t, p, "ship").Deploy.Target.Namespace; ns != "" {
		t.Errorf("namespace = %q, want empty (defaulted downstream)", ns)
	}
}

func TestParse_DeployTarget_Rejections(t *testing.T) {
	tests := []struct{ name, yaml, wantErr string }{
		{
			name: "missing cluster",
			yaml: targetYAML(`application: shop
        sync_mode: trigger`),
			wantErr: "cluster is required",
		},
		{
			name: "missing application",
			yaml: targetYAML(`cluster: prod-hub
        sync_mode: trigger`),
			wantErr: "application is required",
		},
		{
			// Required, never defaulted: an omission must not silently make gocdnext
			// start syncing an Application meant to be observed.
			name: "missing sync_mode",
			yaml: targetYAML(`cluster: prod-hub
        application: shop`),
			wantErr: "sync_mode is required",
		},
		{
			name: "bad sync_mode",
			yaml: targetYAML(`cluster: prod-hub
        application: shop
        sync_mode: push`),
			wantErr: "must be `trigger` or `observe`",
		},
		{
			name: "invalid cluster name",
			yaml: targetYAML(`cluster: Prod_Hub!
        application: shop
        sync_mode: trigger`),
			wantErr: "not a valid cluster name",
		},
		{
			// The gate is the separation-of-duties line — it must never be settable
			// from a file, so the key is not merely ignored, it is rejected.
			name: "governing_gate is not settable from YAML",
			yaml: targetYAML(`cluster: prod-hub
        application: shop
        sync_mode: trigger
        governing_gate: {required: 1}`),
			wantErr: "unknown key",
		},
		{
			name: "unknown key inside target",
			yaml: targetYAML(`cluster: prod-hub
        application: shop
        sync_mode: trigger
        aplication: typo`),
			wantErr: "unknown key",
		},
		{
			name: "target must be an object",
			yaml: `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      environment: production
      target: prod-hub
`,
			wantErr: "must be an object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseNamed(strings.NewReader(tt.yaml), "p", "release")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func targetYAML(body string) string {
	return `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    deploy:
      environment: production
      target:
        ` + body + "\n"
}
