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
