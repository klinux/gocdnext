package scheduler_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// TestBuildAssignment_ComplianceJobIsolatedFromRepoContext is the isolation
// splice: a policy-injected compliance job must NOT inherit repo-controlled
// pipeline variables or services, so a developer can't influence the mandatory
// step from their own YAML. A normal job still gets both.
func TestBuildAssignment_ComplianceJobIsolatedFromRepoContext(t *testing.T) {
	def := domain.Pipeline{
		Name:      "ci",
		Stages:    []string{"build", "_compliance_scan"},
		Variables: map[string]string{"REPO_VAR": "repo-controlled"},
		Services:  []domain.Service{{Name: "db", Image: "postgres:16"}},
		Jobs: []domain.Job{
			{Name: "compile", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}},
			{
				Name: "_compliance_scan", Stage: "_compliance_scan", Image: "scanner",
				Tasks:     []domain.Task{{Script: "scan"}},
				Variables: map[string]string{"POLICY_VAR": "policy-owned"},
			},
		},
	}
	blob, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), ProjectID: uuid.New(),
		ProjectSlug: "shop", Counter: 1, Definition: blob, Cause: "webhook",
	}

	// Normal job: inherits repo pipeline variables + services.
	normal := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "compile"}
	got, _, err := scheduler.BuildAssignment(run, normal, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("BuildAssignment(normal): %v", err)
	}
	if got.Env["REPO_VAR"] != "repo-controlled" {
		t.Errorf("normal job lost repo var: %v", got.Env)
	}
	if len(got.Services) == 0 {
		t.Errorf("normal job lost repo services")
	}

	// Compliance job: repo variables + services are stripped; its own policy
	// variable survives.
	comp := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "_compliance_scan"}
	got2, _, err := scheduler.BuildAssignment(run, comp, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("BuildAssignment(compliance): %v", err)
	}
	if _, leaked := got2.Env["REPO_VAR"]; leaked {
		t.Errorf("repo variable leaked into compliance job: %v", got2.Env)
	}
	if got2.Env["POLICY_VAR"] != "policy-owned" {
		t.Errorf("policy job variable missing: %v", got2.Env)
	}
	if len(got2.Services) != 0 {
		t.Errorf("repo services leaked into compliance job: %v", got2.Services)
	}
}
