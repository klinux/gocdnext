package parser

import (
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
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

func TestValidateNoCycles(t *testing.T) {
	t.Parallel()

	job := func(name string, needs ...string) domain.Job {
		return domain.Job{Name: name, Stage: "build", Needs: needs}
	}

	tests := []struct {
		name      string
		jobs      []domain.Job
		wantCycle bool   // does the validator reject?
		wantNodes string // substring expected in the error trace (empty = don't check trace)
	}{
		{
			name: "no needs anywhere",
			jobs: []domain.Job{job("a"), job("b"), job("c")},
		},
		{
			name: "linear chain a→b→c",
			jobs: []domain.Job{job("a"), job("b", "a"), job("c", "b")},
		},
		{
			name: "diamond (no cycle): d needs b,c; both need a",
			jobs: []domain.Job{job("a"), job("b", "a"), job("c", "a"), job("d", "b", "c")},
		},
		{
			name:      "2-cycle: a→b→a",
			jobs:      []domain.Job{job("a", "b"), job("b", "a")},
			wantCycle: true,
			wantNodes: "a → b → a",
		},
		{
			name:      "3-cycle: a→b→c→a",
			jobs:      []domain.Job{job("a", "c"), job("b", "a"), job("c", "b")},
			wantCycle: true,
			// Deterministic alpha order starts DFS at 'a', traverses
			// 'c' (dep of a), 'b' (dep of c), back to 'a'.
			wantNodes: "a → c → b → a",
		},
		{
			name:      "self-cycle still caught (defensive — validateNeeds rejects too)",
			jobs:      []domain.Job{job("a", "a")},
			wantCycle: true,
			wantNodes: "a → a",
		},
		{
			name: "two disjoint chains, neither cyclic",
			jobs: []domain.Job{
				job("a"), job("b", "a"),
				job("x"), job("y", "x"),
			},
		},
		{
			name: "cycle in one component, other is clean",
			jobs: []domain.Job{
				job("a", "b"), job("b", "a"), // cycle
				job("x"), job("y", "x"), // clean
			},
			wantCycle: true,
			wantNodes: "a → b → a",
		},
		{
			name: "unknown dep skipped (validateNeeds would catch it; we don't double-fail)",
			jobs: []domain.Job{
				job("a", "ghost"),
			},
			// No cycle — visit(a) → ghost not in map → skip → black.
			wantCycle: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateNoCycles(tt.jobs)
			if !tt.wantCycle {
				if err != nil {
					t.Errorf("validateNoCycles returned %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateNoCycles returned nil, want cycle error")
			}
			if !strings.Contains(err.Error(), "cycle detected") {
				t.Errorf("err = %q, want it to mention 'cycle detected'", err.Error())
			}
			if tt.wantNodes != "" && !strings.Contains(err.Error(), tt.wantNodes) {
				t.Errorf("err trace = %q, want substring %q (deterministic cycle path)",
					err.Error(), tt.wantNodes)
			}
		})
	}
}
