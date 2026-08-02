package parser

import (
	"strings"
	"testing"
)

// #206 — a job-level `environment:` declaration makes a change-freeze hold an
// executable job (a prod migration) that carries no `deploy:` marker. The field
// is presence-aware: absent = no env, but an explicitly empty/malformed
// declaration is a loud parse error, never a silent no-env.

func TestParse_Environment_AcceptsOnExecutableJob(t *testing.T) {
	y := `
stages: [migration]
jobs:
  migrate-prod:
    stage: migration
    image: goose
    script: ["goose up"]
    environment: prod
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	job := findJobByName(t, p, "migrate-prod")
	if job.Environment != "prod" {
		t.Errorf("Environment = %q, want prod", job.Environment)
	}
	if got := job.TargetEnvironment(); got != "prod" {
		t.Errorf("TargetEnvironment() = %q, want prod", got)
	}
}

func TestParse_Environment_AbsentIsNoEnv(t *testing.T) {
	y := `
stages: [test]
jobs:
  unit:
    stage: test
    script: ["true"]
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := findJobByName(t, p, "unit").Environment; got != "" {
		t.Errorf("Environment = %q, want empty (absent)", got)
	}
}

func TestParse_Environment_QuotedNumericIsValidString(t *testing.T) {
	y := `
stages: [migration]
jobs:
  migrate:
    stage: migration
    script: ["true"]
    environment: "123"
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := findJobByName(t, p, "migrate").Environment; got != "123" {
		t.Errorf("Environment = %q, want 123 (a quoted string is valid)", got)
	}
}

func TestParse_Environment_EqualToDeployIsAllowed(t *testing.T) {
	y := `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    environment: production
    deploy:
      environment: production
`
	p, err := ParseNamed(strings.NewReader(y), "p", "release")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	job := findJobByName(t, p, "ship")
	if job.Environment != "production" || job.Deploy == nil || job.Deploy.Environment != "production" {
		t.Fatalf("want both environment and deploy.environment = production, got env=%q deploy=%+v", job.Environment, job.Deploy)
	}
	if got := job.TargetEnvironment(); got != "production" {
		t.Errorf("TargetEnvironment() = %q, want production", got)
	}
}

func TestParse_Environment_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "null (declared with no value)",
			yaml: `
stages: [migration]
jobs:
  m:
    stage: migration
    script: ["true"]
    environment:
`,
			wantErr: "environment must not be empty",
		},
		{
			name: "empty string",
			yaml: `
stages: [migration]
jobs:
  m:
    stage: migration
    script: ["true"]
    environment: ""
`,
			wantErr: "environment must not be empty",
		},
		{
			name: "whitespace only",
			yaml: `
stages: [migration]
jobs:
  m:
    stage: migration
    script: ["true"]
    environment: "   "
`,
			wantErr: "environment must not be empty",
		},
		{
			name: "integer (unquoted)",
			yaml: `
stages: [migration]
jobs:
  m:
    stage: migration
    script: ["true"]
    environment: 123
`,
			wantErr: "environment must be a string",
		},
		{
			name: "boolean (unquoted)",
			yaml: `
stages: [migration]
jobs:
  m:
    stage: migration
    script: ["true"]
    environment: true
`,
			wantErr: "environment must be a string",
		},
		{
			name: "sequence",
			yaml: `
stages: [migration]
jobs:
  m:
    stage: migration
    script: ["true"]
    environment: [prod]
`,
			wantErr: "environment must be a string",
		},
		{
			name: "forbidden characters",
			yaml: `
stages: [migration]
jobs:
  m:
    stage: migration
    script: ["true"]
    environment: "prod; rm -rf"
`,
			wantErr: "forbidden characters",
		},
		{
			name: "on a non-executable (no-op) job",
			yaml: `
stages: [migration]
jobs:
  m:
    stage: migration
    environment: prod
`,
			wantErr: "requires an executable job",
		},
		{
			name: "on an approval gate",
			yaml: `
stages: [approve]
jobs:
  g:
    stage: approve
    approval:
      approvers: [alice]
    environment: prod
`,
			wantErr: "approval gate cannot declare",
		},
		{
			name: "differs from deploy.environment",
			yaml: `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: ["true"]
    environment: production
    deploy:
      environment: staging
`,
			wantErr: "must equal deploy.environment",
		},
		{
			name: "typo'd key rejected by KnownFields",
			yaml: `
stages: [migration]
jobs:
  m:
    stage: migration
    script: ["true"]
    environmnet: prod
`,
			wantErr: "environmnet",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseNamed(strings.NewReader(tt.yaml), "p", "release")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestEmit_RoundTripsEnvironment(t *testing.T) {
	const src = `
name: demo
stages: [migration]
materials: [{ manual: true }]
jobs:
  migrate:
    stage: migration
    image: goose
    script: ["goose up"]
    environment: prod
`
	first, err := ParseNamed(strings.NewReader(src), "p", "demo")
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	out, err := Emit(first)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	second, err := ParseNamed(strings.NewReader(string(out)), "p", "demo")
	if err != nil {
		t.Fatalf("re-parse: %v\n---\n%s", err, out)
	}
	for _, p := range []struct {
		label string
		env   string
	}{
		{"first", findJobByName(t, first, "migrate").Environment},
		{"second", findJobByName(t, second, "migrate").Environment},
	} {
		if p.env != "prod" {
			t.Errorf("%s: Environment = %q, want prod (round-trip)\n---\n%s", p.label, p.env, out)
		}
	}
}

// Extends: the new field must be child-wins-or-zero (#206) — base inheritance is
// deferred to #209. A DIRECT declaration on the child works; a base's declaration
// does NOT leak to a child that omits it.

func TestParse_Environment_ExtendsChildDeclarationKept(t *testing.T) {
	y := `
stages: [migration]
materials: [{manual: true}]
jobs:
  .base-mig:
    stage: migration
    image: goose
    script: ["goose up"]
  migrate-prod:
    extends: .base-mig
    environment: prod
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := findJobByName(t, p, "migrate-prod").Environment; got != "prod" {
		t.Errorf("Environment = %q, want prod (child's own declaration kept)", got)
	}
}

func TestParse_Environment_ExtendsBaseNotInherited(t *testing.T) {
	y := `
stages: [migration]
materials: [{manual: true}]
jobs:
  .base-env:
    stage: migration
    image: goose
    script: ["goose up"]
    environment: prod
  plain:
    extends: .base-env
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := findJobByName(t, p, "plain").Environment; got != "" {
		t.Errorf("Environment = %q, want empty — base environment: must NOT be inherited (that's #209)", got)
	}
}
