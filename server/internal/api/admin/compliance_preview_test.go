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
			Jobs []previewJob `json:"jobs"`
		} `json:"raw"`
		Effective struct {
			Jobs []previewJob `json:"jobs"`
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

// previewJob decodes one job from the lower-cased preview DTO.
type previewJob struct {
	Name  string `json:"name"`
	Stage string `json:"stage"`
}

func hasJobNamed(jobs []previewJob, name string) bool {
	for _, j := range jobs {
		if j.Name == name {
			return true
		}
	}
	return false
}

// --- draft-policy preview (POST /compliance/preview-policy) ----------------

func previewBody(t *testing.T, m map[string]any) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewBuffer(b)
}

type previewResp struct {
	Raw struct {
		Stages []string `json:"stages"`
	} `json:"raw"`
	Effective struct {
		Stages []string `json:"stages"`
		Jobs   []struct {
			Name  string `json:"name"`
			Stage string `json:"stage"`
		} `json:"jobs"`
	} `json:"effective"`
}

const previewYAML = "stages: [_compliance_scan]\n" +
	"jobs:\n  _compliance_scan:\n    stage: _compliance_scan\n    image: scanner\n    script: [\"scan\"]\n"

// The draft is previewed against a REAL project's pipelines. seedGovernedProject
// (assign=false) gives project "payments" a `main` pipeline with stages [build]
// + an SCM source; framework_ids:[] in the body means "no saved policies", so
// the effective definition is the real pipeline with only the draft merged in.
func TestPreviewDraftPolicy_Inject(t *testing.T) {
	srv, s := newComplianceHandlerStore(t)
	seedGovernedProject(t, s, "payments", false)
	rr := request(srv, http.MethodPost, "/api/v1/admin/compliance/preview-policy",
		previewBody(t, map[string]any{
			"slug": "payments", "framework_ids": []string{},
			"config_yaml": previewYAML, "mode": "inject", "position_after": "build",
		}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out []previewResp
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 pipeline view, got %d", len(out))
	}
	// Raw is the project's real pipeline; effective inserts the draft after build.
	if got := out[0].Raw.Stages; !equalStrings(got, []string{"build"}) {
		t.Fatalf("raw stages = %v, want [build]", got)
	}
	if got := out[0].Effective.Stages; !equalStrings(got, []string{"build", "_compliance_scan"}) {
		t.Fatalf("effective stages = %v, want [build _compliance_scan]", got)
	}
}

func TestPreviewDraftPolicy_Override(t *testing.T) {
	srv, s := newComplianceHandlerStore(t)
	seedGovernedProject(t, s, "payments", false)
	rr := request(srv, http.MethodPost, "/api/v1/admin/compliance/preview-policy",
		previewBody(t, map[string]any{
			"slug": "payments", "framework_ids": []string{},
			"config_yaml": previewYAML, "mode": "override",
		}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out []previewResp
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out) != 1 || !equalStrings(out[0].Effective.Stages, []string{"_compliance_scan"}) {
		t.Fatalf("override effective = %+v, want one pipeline with [_compliance_scan]", out)
	}
}

func TestPreviewDraftPolicy_Rejects(t *testing.T) {
	srv, s := newComplianceHandlerStore(t)
	seedGovernedProject(t, s, "payments", false)
	cases := map[string]map[string]any{
		"missing slug":   {"config_yaml": previewYAML},
		"invalid yaml":   {"slug": "payments", "config_yaml": "stages: [oops\n"},
		"missing prefix": {"slug": "payments", "config_yaml": "stages: [scan]\njobs:\n  scan: {stage: scan, image: x, script: [\"y\"]}\n"},
		"bad mode":       {"slug": "payments", "config_yaml": previewYAML, "mode": "bogus"},
		"both positions": {"slug": "payments", "config_yaml": previewYAML, "position_before": "a", "position_after": "b"},
		"bad framework":  {"slug": "payments", "framework_ids": []string{"not-a-uuid"}, "config_yaml": previewYAML},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rr := request(srv, http.MethodPost, "/api/v1/admin/compliance/preview-policy", previewBody(t, body))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rr.Code, rr.Body.String())
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
