package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func newComplianceHandler(t *testing.T) http.Handler {
	t.Helper()
	h, _ := newComplianceHandlerStore(t)
	return h
}

func newComplianceHandlerStore(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	s := store.New(dbtest.SetupPool(t))
	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	return mount(h), s
}

func TestComplianceFrameworkHTTP_CRUD(t *testing.T) {
	srv := newComplianceHandler(t)

	rr := request(srv, http.MethodPost, "/api/v1/admin/compliance/frameworks",
		bytes.NewBufferString(`{"name":"SOC2","description":"soc2"}`))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create framework status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var fw struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &fw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fw.ID == "" || fw.Name != "SOC2" {
		t.Fatalf("unexpected framework: %+v", fw)
	}

	rr = request(srv, http.MethodGet, "/api/v1/admin/compliance/frameworks", nil)
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte("SOC2")) {
		t.Fatalf("list status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Unused framework deletes cleanly.
	rr = request(srv, http.MethodDelete, "/api/v1/admin/compliance/frameworks/"+fw.ID, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCompliancePolicyHTTP_EnforcementDropMapsTo409(t *testing.T) {
	srv, s := newComplianceHandlerStore(t)
	ctx := context.Background()

	// A project with a pipeline but NO scm binding can't be enforced.
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "nobind", Name: "nobind",
		Pipelines: []*domain.Pipeline{{
			Name: "main", Stages: []string{"build"}, Jobs: []domain.Job{{Name: "compile", Stage: "build"}},
		}},
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Creating an applies_to_all policy would govern that unenforceable project
	// → the store returns ErrComplianceWouldDropEnforcement, which the admin
	// route must surface as 409 (not 500).
	body := `{"name":"global","mode":"inject","enabled":true,"applies_to_all":true,
	  "config_yaml":"stages: [_compliance_scan]\njobs:\n  _compliance_scan:\n    stage: _compliance_scan\n    image: s\n    script: [\"s\"]\n"}`
	rr := request(srv, http.MethodPost, "/api/v1/admin/compliance/policies",
		bytes.NewBufferString(body))
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCompliancePolicyHTTP_RejectsBadConfig(t *testing.T) {
	srv := newComplianceHandler(t)

	// A policy whose names are not reserved-prefixed is a 400 (author error).
	body := `{"name":"bad","mode":"inject","enabled":true,
	  "config_yaml":"stages: [scan]\njobs:\n  scan:\n    stage: scan\n    image: x\n    script: [\"s\"]\n"}`
	rr := request(srv, http.MethodPost, "/api/v1/admin/compliance/policies",
		bytes.NewBufferString(body))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad config, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCompliancePolicyHTTP_CreateAndGet(t *testing.T) {
	srv := newComplianceHandler(t)

	body := `{"name":"pci-scan","mode":"inject","enabled":true,
	  "config_yaml":"stages: [_compliance_scan]\njobs:\n  _compliance_scan:\n    stage: _compliance_scan\n    image: scanner\n    script: [\"scan\"]\n"}`
	rr := request(srv, http.MethodPost, "/api/v1/admin/compliance/policies",
		bytes.NewBufferString(body))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create policy status=%d body=%s", rr.Code, rr.Body.String())
	}
	var p struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Mode       string `json:"mode"`
		ConfigYAML string `json:"config_yaml"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.ID == "" || p.Mode != "inject" {
		t.Fatalf("unexpected policy: %+v", p)
	}

	rr = request(srv, http.MethodGet, "/api/v1/admin/compliance/policies/"+p.ID, nil)
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte("_compliance_scan")) {
		t.Fatalf("get policy status=%d body=%s", rr.Code, rr.Body.String())
	}

	// 404 for an unknown id.
	rr = request(srv, http.MethodGet,
		"/api/v1/admin/compliance/policies/00000000-0000-0000-0000-000000000000", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}
