package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func previewJobNames(p domain.Pipeline) []string {
	out := make([]string, len(p.Jobs))
	for i, j := range p.Jobs {
		out[i] = j.Name
	}
	return out
}

// viewByName finds a preview entry by pipeline name.
func viewByName(t *testing.T, views []store.EffectivePipelineView, name string) store.EffectivePipelineView {
	t.Helper()
	for _, v := range views {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no preview entry named %q in %+v", name, views)
	return store.EffectivePipelineView{}
}

func TestPreviewEffective_StoredUngoverned(t *testing.T) {
	s, ctx := newComplianceStore(t)
	applyDemoProject(t, s, ctx, "payments")

	views, err := s.PreviewEffectivePipelines(ctx, "payments", nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("want 1 pipeline, got %d: %+v", len(views), views)
	}
	v := viewByName(t, views, "main")
	if v.SystemManaged {
		t.Errorf("repo pipeline must not be system_managed")
	}
	// Ungoverned: effective == raw, both carry exactly the repo's own jobs.
	if got := previewJobNames(v.Effective); !hasJob(got, "compile") || !hasJob(got, "ship") {
		t.Errorf("effective jobs = %v, want compile+ship", got)
	}
	if hasJob(previewJobNames(v.Effective), "_compliance_scan") {
		t.Errorf("ungoverned pipeline must not carry a compliance job: %v", previewJobNames(v.Effective))
	}
	if len(previewJobNames(v.Raw)) != len(previewJobNames(v.Effective)) {
		t.Errorf("ungoverned: raw and effective should match (raw=%v eff=%v)",
			previewJobNames(v.Raw), previewJobNames(v.Effective))
	}
}

func TestPreviewEffective_StoredGoverned(t *testing.T) {
	s, ctx := newComplianceStore(t)
	applyDemoProject(t, s, ctx, "payments")
	fw := mustFramework(t, s, ctx, "PCI")
	mustPolicy(t, s, ctx, "pci-scan", []string{fw.ID})
	mustAssign(t, s, ctx, "payments", []string{fw.ID})

	views, err := s.PreviewEffectivePipelines(ctx, "payments", nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	v := viewByName(t, views, "main")
	if !hasJob(previewJobNames(v.Effective), "_compliance_scan") {
		t.Errorf("governed effective missing policy job: %v", previewJobNames(v.Effective))
	}
	if hasJob(previewJobNames(v.Raw), "_compliance_scan") {
		t.Errorf("raw (pre-policy) must NOT carry the policy job: %v", previewJobNames(v.Raw))
	}
}

func TestPreviewEffective_WhatIfAddsGovernanceWithoutPersisting(t *testing.T) {
	s, ctx := newComplianceStore(t)
	applyDemoProject(t, s, ctx, "payments")
	fw := mustFramework(t, s, ctx, "PCI")
	mustPolicy(t, s, ctx, "pci-scan", []string{fw.ID})
	// NB: the framework is NOT assigned to the project.

	whatIf := []string{fw.ID}
	views, err := s.PreviewEffectivePipelines(ctx, "payments", &whatIf)
	if err != nil {
		t.Fatalf("what-if preview: %v", err)
	}
	v := viewByName(t, views, "main")
	if !hasJob(previewJobNames(v.Effective), "_compliance_scan") {
		t.Errorf("what-if effective should add the policy job: %v", previewJobNames(v.Effective))
	}

	// What-if must not have persisted anything: the stored read is still clean.
	stored, err := s.PreviewEffectivePipelines(ctx, "payments", nil)
	if err != nil {
		t.Fatalf("stored preview: %v", err)
	}
	if hasJob(previewJobNames(viewByName(t, stored, "main").Effective), "_compliance_scan") {
		t.Errorf("what-if leaked into stored state: %v", previewJobNames(viewByName(t, stored, "main").Effective))
	}
}

func TestPreviewEffective_WhatIfEmptyClearsGovernance(t *testing.T) {
	s, ctx := newComplianceStore(t)
	applyDemoProject(t, s, ctx, "payments")
	fw := mustFramework(t, s, ctx, "PCI")
	mustPolicy(t, s, ctx, "pci-scan", []string{fw.ID})
	mustAssign(t, s, ctx, "payments", []string{fw.ID})

	// Empty (but non-nil) framework set → only global policies apply (none here),
	// so the preview shows the pipeline WITHOUT the framework-scoped policy job.
	empty := []string{}
	views, err := s.PreviewEffectivePipelines(ctx, "payments", &empty)
	if err != nil {
		t.Fatalf("what-if preview: %v", err)
	}
	if hasJob(previewJobNames(viewByName(t, views, "main").Effective), "_compliance_scan") {
		t.Errorf("clearing frameworks should drop the policy job in the preview: %v",
			previewJobNames(viewByName(t, views, "main").Effective))
	}
}

func TestPreviewEffective_StoredSynthetic(t *testing.T) {
	s, ctx := newComplianceStore(t)
	applyWithSCM(t, s, ctx, "svc", nil) // pipeline-less project with an scm binding
	fw := mustFramework(t, s, ctx, "SOC2")
	mustPolicy(t, s, ctx, "soc2-scan", []string{fw.ID})
	mustAssign(t, s, ctx, "svc", []string{fw.ID})

	views, err := s.PreviewEffectivePipelines(ctx, "svc", nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("want exactly the synthetic pipeline, got %d: %+v", len(views), views)
	}
	v := viewByName(t, views, "_compliance")
	if !v.SystemManaged {
		t.Errorf("synthetic pipeline must be flagged system_managed")
	}
	if !hasJob(previewJobNames(v.Effective), "_compliance_scan") {
		t.Errorf("synthetic effective missing policy job: %v", previewJobNames(v.Effective))
	}
	if len(v.Raw.Jobs) != 0 {
		t.Errorf("synthetic raw skeleton should carry no jobs of its own: %v", previewJobNames(v.Raw))
	}
}

func TestPreviewEffective_WhatIfSyntheticForPipelinelessProject(t *testing.T) {
	s, ctx := newComplianceStore(t)
	applyWithSCM(t, s, ctx, "svc", nil) // pipeline-less, ungoverned
	fw := mustFramework(t, s, ctx, "SOC2")
	mustPolicy(t, s, ctx, "soc2-scan", []string{fw.ID})

	whatIf := []string{fw.ID}
	views, err := s.PreviewEffectivePipelines(ctx, "svc", &whatIf)
	if err != nil {
		t.Fatalf("what-if preview: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("want a synthetic preview entry, got %d: %+v", len(views), views)
	}
	v := viewByName(t, views, "_compliance")
	if !v.SystemManaged || !hasJob(previewJobNames(v.Effective), "_compliance_scan") {
		t.Errorf("what-if synthetic wrong: system_managed=%v jobs=%v", v.SystemManaged, previewJobNames(v.Effective))
	}
}

func TestPreviewEffective_UnknownProject(t *testing.T) {
	s, ctx := newComplianceStore(t)
	if _, err := s.PreviewEffectivePipelines(ctx, "ghost", nil); !errors.Is(err, store.ErrProjectNotFound) {
		t.Fatalf("want ErrProjectNotFound, got %v", err)
	}
}

// The what-if preview must refuse a hypothetical governance the real save would
// reject — a governed project with no SCM source can't be enforced (the trigger
// is push-driven). Otherwise the preview would show a state assignment rejects.
func TestPreviewEffective_WhatIfRefusesRepoPipelineWithoutSCM(t *testing.T) {
	s, ctx := newComplianceStore(t)
	// A project WITH a pipeline but NO scm binding. Ungoverned at apply time
	// (the framework below isn't assigned), so ApplyProject succeeds.
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "nobind", Name: "nobind",
		Pipelines: []*domain.Pipeline{{
			Name: "main", Stages: []string{"build"},
			Jobs: []domain.Job{{Name: "compile", Stage: "build"}},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	fw := mustFramework(t, s, ctx, "PCI")
	mustPolicy(t, s, ctx, "pci-scan", []string{fw.ID})

	whatIf := []string{fw.ID}
	if _, err := s.PreviewEffectivePipelines(ctx, "nobind", &whatIf); !errors.Is(err, store.ErrComplianceWouldDropEnforcement) {
		t.Fatalf("want ErrComplianceWouldDropEnforcement, got %v", err)
	}
}

func TestPreviewEffective_WhatIfRefusesPipelinelessWithoutSCM(t *testing.T) {
	s, ctx := newComplianceStore(t)
	// Empty project, no scm binding, ungoverned → ApplyProject succeeds.
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "empty", Name: "empty"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	fw := mustFramework(t, s, ctx, "PCI")
	mustPolicy(t, s, ctx, "pci-scan", []string{fw.ID})

	whatIf := []string{fw.ID}
	if _, err := s.PreviewEffectivePipelines(ctx, "empty", &whatIf); !errors.Is(err, store.ErrComplianceWouldDropEnforcement) {
		t.Fatalf("want ErrComplianceWouldDropEnforcement, got %v", err)
	}
}

// --- small builders kept local to the preview suite ---

func mustFramework(t *testing.T, s *store.Store, ctx context.Context, name string) store.ComplianceFramework {
	t.Helper()
	fw, err := s.InsertComplianceFramework(ctx, store.FrameworkInput{Name: name})
	if err != nil {
		t.Fatalf("framework %s: %v", name, err)
	}
	return fw
}

func mustPolicy(t *testing.T, s *store.Store, ctx context.Context, name string, frameworkIDs []string) {
	t.Helper()
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: name, Mode: "inject", Enabled: true,
		FrameworkIDs: frameworkIDs, ConfigYAML: scanPolicyYAML,
	}); err != nil {
		t.Fatalf("policy %s: %v", name, err)
	}
}

func mustAssign(t *testing.T, s *store.Store, ctx context.Context, slug string, frameworkIDs []string) {
	t.Helper()
	pid, err := uuid.Parse(mustProjectID(t, s, ctx, slug))
	if err != nil {
		t.Fatalf("parse project id: %v", err)
	}
	if err := s.SetProjectFrameworks(ctx, pid, frameworkIDs); err != nil {
		t.Fatalf("assign frameworks to %s: %v", slug, err)
	}
}
