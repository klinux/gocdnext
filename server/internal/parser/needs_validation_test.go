package parser

import (
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

func TestValidateNeeds(t *testing.T) {
	t.Parallel()

	job := func(name, stage string, needs ...string) domain.Job {
		return domain.Job{Name: name, Stage: stage, Needs: needs}
	}

	tests := []struct {
		name    string
		jobs    []domain.Job
		stages  []string
		wantErr string // substring; empty = no error expected
	}{
		{
			name:   "no needs anywhere",
			jobs:   []domain.Job{job("a", "build"), job("b", "build")},
			stages: []string{"build"},
		},
		{
			name:   "same-stage backward valid",
			jobs:   []domain.Job{job("a", "build"), job("b", "build", "a")},
			stages: []string{"build"},
		},
		{
			name:   "cross-stage backward valid (earlier stage)",
			jobs:   []domain.Job{job("a", "build"), job("b", "deploy", "a")},
			stages: []string{"build", "deploy"},
		},
		{
			name:   "multi-needs all valid",
			jobs:   []domain.Job{job("a", "build"), job("b", "build"), job("c", "build", "a", "b")},
			stages: []string{"build"},
		},
		{
			name:    "self-reference rejected",
			jobs:    []domain.Job{job("a", "build", "a")},
			stages:  []string{"build"},
			wantErr: `job "a": ` + "`needs:`" + ` contains itself`,
		},
		{
			name:    "unknown job rejected",
			jobs:    []domain.Job{job("a", "build", "ghost")},
			stages:  []string{"build"},
			wantErr: `unknown job "ghost"`,
		},
		{
			name:    "forward-stage rejected",
			jobs:    []domain.Job{job("a", "build", "b"), job("b", "deploy")},
			stages:  []string{"build", "deploy"},
			wantErr: `forward references would deadlock`,
		},
		{
			name: "user reported: build needs siblings, all in build stage",
			jobs: []domain.Job{
				job("eslint", "build"),
				job("typecheck", "build"),
				job("unit", "build"),
				job("types-generate", "build"),
				job("build", "build", "eslint", "typecheck", "unit", "types-generate"),
			},
			stages: []string{"build"},
		},
		{
			name: "matrix-style: needs by name (matrix children covered by name)",
			jobs: []domain.Job{
				job("test", "test"), // implicitly matrix in real life, but parser sees one name
				job("deploy", "deploy", "test"),
			},
			stages: []string{"test", "deploy"},
		},
		{
			name: "typo in needs caught (would be silent-skip without validator)",
			jobs: []domain.Job{
				job("typescheck", "build"), // intentional typo: typesCheck not typeCheck
				job("build", "build", "typecheck"),
			},
			stages:  []string{"build"},
			wantErr: `unknown job "typecheck"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateNeeds(tt.jobs, tt.stages)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validateNeeds returned %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateNeeds returned nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validateNeeds err = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}
