package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func newComplianceStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	s := store.New(dbtest.SetupPool(t))
	s.SetAuthCipher(newAuthCipher(t)) // ApplyProject with an scm seals the webhook secret
	return s, context.Background()
}

const scanPolicyYAML = `
stages: [_compliance_scan]
jobs:
  _compliance_scan:
    stage: _compliance_scan
    image: scanner:latest
    script: ["scan ."]
`

// applyDemoProject applies a minimal project with one pipeline (build→deploy)
// and returns the project id + the pipeline id.
func applyDemoProject(t *testing.T, s *store.Store, ctx context.Context, slug string) (string, string) {
	t.Helper()
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		SCMSource: &store.SCMSourceInput{
			Provider: "github", URL: "https://github.com/acme/" + slug, DefaultBranch: "main",
		},
		Pipelines: []*domain.Pipeline{{
			Name:   "main",
			Stages: []string{"build", "deploy"},
			Jobs: []domain.Job{
				{Name: "compile", Stage: "build"},
				{Name: "ship", Stage: "deploy"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply project: %v", err)
	}
	return res.ProjectID.String(), res.Pipelines[0].PipelineID.String()
}

func effectiveJobNames(t *testing.T, s *store.Store, ctx context.Context, pipelineID string) []string {
	t.Helper()
	id, err := uuid.Parse(pipelineID)
	if err != nil {
		t.Fatalf("parse pipeline id: %v", err)
	}
	p, err := s.GetPipelineByID(ctx, id)
	if err != nil {
		t.Fatalf("get pipeline: %v", err)
	}
	names := make([]string, len(p.Jobs))
	for i, j := range p.Jobs {
		names[i] = j.Name
	}
	return names
}

func hasJob(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestComplianceFrameworkCRUDAndUsageGuard(t *testing.T) {
	s, ctx := newComplianceStore(t)

	fw, err := s.InsertComplianceFramework(ctx, store.FrameworkInput{Name: "SOC2", Description: "soc2"})
	if err != nil {
		t.Fatalf("insert framework: %v", err)
	}
	list, err := s.ListComplianceFrameworks(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "SOC2" {
		t.Fatalf("list frameworks = %v, err=%v", list, err)
	}

	// Unused framework: usage zero, delete allowed.
	usage, err := s.FrameworkUsage(ctx, fw.ID)
	if err != nil || usage.Projects != 0 || usage.Policies != 0 {
		t.Fatalf("usage = %+v, err=%v", usage, err)
	}
	if err := s.DeleteComplianceFramework(ctx, fw.ID); err != nil {
		t.Fatalf("delete framework: %v", err)
	}
}

func TestCompliancePolicyValidation(t *testing.T) {
	s, ctx := newComplianceStore(t)

	// A policy whose job/stage names are not reserved-prefixed must be rejected.
	_, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "bad", Mode: "inject", Enabled: true,
		ConfigYAML: "stages: [scan]\njobs:\n  scan:\n    stage: scan\n    image: x\n    script: [\"s\"]\n",
	})
	if err == nil || !strings.Contains(err.Error(), "_compliance_") {
		t.Fatalf("expected reserved-prefix rejection, got %v", err)
	}

	// Invalid mode rejected.
	_, err = s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "bad2", Mode: "weird", Enabled: true, ConfigYAML: scanPolicyYAML,
	})
	if err == nil {
		t.Fatal("expected invalid-mode rejection")
	}
}

func TestComplianceEnforcementMergesMandatoryJob(t *testing.T) {
	s, ctx := newComplianceStore(t)
	_, pipelineID := applyDemoProject(t, s, ctx, "payments")

	// Before any framework: effective == raw, no compliance job.
	if names := effectiveJobNames(t, s, ctx, pipelineID); hasJob(names, "_compliance_scan") {
		t.Fatalf("unexpected compliance job before enforcement: %v", names)
	}

	fw, err := s.InsertComplianceFramework(ctx, store.FrameworkInput{Name: "PCI"})
	if err != nil {
		t.Fatalf("framework: %v", err)
	}
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "pci-scan", Mode: "inject", Enabled: true,
		FrameworkIDs: []string{fw.ID}, ConfigYAML: scanPolicyYAML,
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}

	// Assigning the framework recomputes the project's effective definition.
	pid, _ := uuid.Parse(mustProjectID(t, s, ctx, "payments"))
	if err := s.SetProjectFrameworks(ctx, pid, []string{fw.ID}); err != nil {
		t.Fatalf("assign framework: %v", err)
	}

	names := effectiveJobNames(t, s, ctx, pipelineID)
	if !hasJob(names, "_compliance_scan") {
		t.Fatalf("mandatory job not enforced after assignment: %v", names)
	}
	// The project's own jobs survive (inject, not replace).
	if !hasJob(names, "compile") || !hasJob(names, "ship") {
		t.Fatalf("repo jobs lost: %v", names)
	}
}

func TestCompliancePolicyCollisionRejected(t *testing.T) {
	s, ctx := newComplianceStore(t)
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "a", Mode: "inject", Enabled: true, ConfigYAML: scanPolicyYAML,
	}); err != nil {
		t.Fatalf("first policy: %v", err)
	}
	// A second policy reusing the same reserved job name must be rejected —
	// otherwise it would materialise duplicate job_runs.
	_, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "b", Mode: "inject", Enabled: true, ConfigYAML: scanPolicyYAML,
	})
	if err == nil || !strings.Contains(err.Error(), "already defined by policy") {
		t.Fatalf("expected collision rejection, got %v", err)
	}
}

func TestComplianceRepoCannotShadowReservedPrefix(t *testing.T) {
	s, ctx := newComplianceStore(t)
	_, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "evil", Name: "evil",
		Pipelines: []*domain.Pipeline{{
			Name:   "main",
			Stages: []string{"build"},
			Jobs:   []domain.Job{{Name: "_compliance_fake", Stage: "build"}},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "_compliance_") {
		t.Fatalf("repo using reserved prefix should be rejected, got %v", err)
	}
}

func mustProjectID(t *testing.T, s *store.Store, ctx context.Context, slug string) string {
	t.Helper()
	id, err := s.ProjectIDBySlug(ctx, slug)
	if err != nil {
		t.Fatalf("project id by slug: %v", err)
	}
	return id.String()
}
