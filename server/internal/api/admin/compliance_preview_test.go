package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedGovernedProject applies a project with an scm binding + one repo pipeline,
// then a framework-scoped policy. Returns the framework id. The project is
// governed only when assign=true.
func seedGovernedProject(t *testing.T, s *store.Store, slug string, assign bool) string {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		SCMSource: &store.SCMSourceInput{
			Provider: "github", URL: "https://github.com/acme/" + slug, DefaultBranch: "main",
		},
		Pipelines: []*domain.Pipeline{{
			Name: "main", Stages: []string{"build"},
			Jobs: []domain.Job{{Name: "compile", Stage: "build"}},
		}},
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	fw, err := s.InsertComplianceFramework(ctx, store.FrameworkInput{Name: "PCI"})
	if err != nil {
		t.Fatalf("framework: %v", err)
	}
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "pci-scan-" + slug, Mode: "inject", Enabled: true, FrameworkIDs: []string{fw.ID},
		ConfigYAML: "stages: [_compliance_scan]\njobs:\n  _compliance_scan:\n    stage: _compliance_scan\n    image: scanner\n    script: [\"scan\"]\n",
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if assign {
		pid, err := s.ProjectIDBySlug(ctx, slug)
		if err != nil {
			t.Fatalf("project id: %v", err)
		}
		if err := s.SetProjectFrameworks(ctx, pid, []string{fw.ID}); err != nil {
			t.Fatalf("assign: %v", err)
		}
	}
	return fw.ID
}

func TestEffectivePipelinePreviewHTTP_Stored(t *testing.T) {
	srv, s := newComplianceHandlerStore(t)
	seedGovernedProject(t, s, "payments", true)

	rr := request(srv, http.MethodGet, "/api/v1/admin/projects/payments/effective-pipeline", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var views []struct {
		Name          string `json:"name"`
		SystemManaged bool   `json:"system_managed"`
		Raw           struct {
			Jobs []domain.Job `json:"Jobs"`
		} `json:"raw"`
		Effective struct {
			Jobs []domain.Job `json:"Jobs"`
		} `json:"effective"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if len(views) != 1 || views[0].Name != "main" {
		t.Fatalf("unexpected views: %+v", views)
	}
	if !hasJobNamed(views[0].Effective.Jobs, "_compliance_scan") {
		t.Errorf("effective should carry the policy job: %+v", views[0].Effective.Jobs)
	}
	if hasJobNamed(views[0].Raw.Jobs, "_compliance_scan") {
		t.Errorf("raw should NOT carry the policy job: %+v", views[0].Raw.Jobs)
	}
}

func TestEffectivePipelinePreviewHTTP_WhatIf(t *testing.T) {
	srv, s := newComplianceHandlerStore(t)
	fw := seedGovernedProject(t, s, "payments", false) // ungoverned

	// Stored: no compliance job yet.
	rr := request(srv, http.MethodGet, "/api/v1/admin/projects/payments/effective-pipeline", nil)
	if rr.Code != http.StatusOK || bytes.Contains(rr.Body.Bytes(), []byte("_compliance_scan")) {
		t.Fatalf("stored should be clean: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// What-if with the framework assigned: the policy job appears.
	rr = request(srv, http.MethodGet,
		"/api/v1/admin/projects/payments/effective-pipeline?frameworks="+fw, nil)
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte("_compliance_scan")) {
		t.Fatalf("what-if should add the policy job: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestEffectivePipelinePreviewHTTP_UnknownProjectIs404(t *testing.T) {
	srv := newComplianceHandler(t)
	rr := request(srv, http.MethodGet, "/api/v1/admin/projects/ghost/effective-pipeline", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestEffectivePipelinePreviewHTTP_InvalidFrameworkIs400(t *testing.T) {
	srv, s := newComplianceHandlerStore(t)
	seedGovernedProject(t, s, "payments", false)
	rr := request(srv, http.MethodGet,
		"/api/v1/admin/projects/payments/effective-pipeline?frameworks=not-a-uuid", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed framework id, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func hasJobNamed(jobs []domain.Job, name string) bool {
	for _, j := range jobs {
		if j.Name == name {
			return true
		}
	}
	return false
}
