package compliance_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/cli/internal/compliance"
)

func TestListFrameworks_PathAndDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/compliance/frameworks" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]compliance.Framework{
			{ID: "fw-1", Name: "PCI", Description: "card data"},
			{ID: "fw-2", Name: "SOC2"},
		})
	}))
	defer srv.Close()

	got, err := compliance.ListFrameworks(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("ListFrameworks: %v", err)
	}
	if len(got) != 2 || got[0].Name != "PCI" || got[0].ID != "fw-1" {
		t.Fatalf("got = %+v", got)
	}
}

func TestListPolicies_PathAndDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/compliance/policies" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]compliance.Policy{
			{Name: "pci-scan", Mode: "inject", Priority: 0, Enabled: true},
		})
	}))
	defer srv.Close()

	got, err := compliance.ListPolicies(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	if len(got) != 1 || got[0].Mode != "inject" {
		t.Fatalf("got = %+v", got)
	}
}

func TestEffectivePipeline_StoredHasNoFrameworksQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/projects/payments/effective-pipeline" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Has("frameworks") {
			t.Errorf("stored preview must not send a frameworks query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]compliance.PipelineView{
			{Name: "main"},
		})
	}))
	defer srv.Close()

	got, err := compliance.EffectivePipeline(context.Background(), srv.Client(), srv.URL, "payments", nil)
	if err != nil {
		t.Fatalf("EffectivePipeline: %v", err)
	}
	if len(got) != 1 || got[0].Name != "main" {
		t.Fatalf("got = %+v", got)
	}
}

func TestEffectivePipeline_WhatIfSendsFrameworks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("frameworks"); got != "a,b" {
			t.Errorf("frameworks = %q, want a,b", got)
		}
		_ = json.NewEncoder(w).Encode([]compliance.PipelineView{})
	}))
	defer srv.Close()

	whatIf := "a,b"
	if _, err := compliance.EffectivePipeline(context.Background(), srv.Client(), srv.URL, "payments", &whatIf); err != nil {
		t.Fatalf("EffectivePipeline: %v", err)
	}
}

func TestEffectivePipeline_WhatIfEmptyStillSendsParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Empty (but present) means "no frameworks" — the server distinguishes
		// absent (stored) from present-empty (what-if with nothing assigned).
		if !r.URL.Query().Has("frameworks") {
			t.Errorf("empty what-if must still send the frameworks param: %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]compliance.PipelineView{})
	}))
	defer srv.Close()

	empty := ""
	if _, err := compliance.EffectivePipeline(context.Background(), srv.Client(), srv.URL, "payments", &empty); err != nil {
		t.Fatalf("EffectivePipeline: %v", err)
	}
}

func TestEffectivePipeline_DecodesEnforcedJobs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Explicit lower-case preview DTO from the endpoint.
		_, _ = w.Write([]byte(`[{"name":"_compliance","system_managed":true,
		  "raw":{"stages":[],"jobs":[]},
		  "effective":{"stages":["_compliance_scan"],"jobs":[{"name":"_compliance_scan","stage":"_compliance_scan"}]}}]`))
	}))
	defer srv.Close()

	got, err := compliance.EffectivePipeline(context.Background(), srv.Client(), srv.URL, "svc", nil)
	if err != nil {
		t.Fatalf("EffectivePipeline: %v", err)
	}
	if len(got) != 1 || !got[0].SystemManaged {
		t.Fatalf("got = %+v", got)
	}
	if len(got[0].Effective.Jobs) != 1 || got[0].Effective.Jobs[0].Name != "_compliance_scan" {
		t.Fatalf("effective jobs = %+v", got[0].Effective.Jobs)
	}
}

func TestGet_Non2xxIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := compliance.EffectivePipeline(context.Background(), srv.Client(), srv.URL, "ghost", nil)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}
