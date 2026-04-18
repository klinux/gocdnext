package projects_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// sampleFile is the smallest YAML accepted by the parser: one stage, one job.
const sampleFile = `name: build
materials:
  - git:
      url: https://github.com/org/demo
      branch: main
stages: [build]
jobs:
  compile:
    stage: build
    script: [echo hi]
`

func newHandler(t *testing.T) (*projects.Handler, *store.Store) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	return projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))), s
}

func doApply(t *testing.T, h *projects.Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/apply", &buf)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Apply(rr, req)
	return rr
}

func TestApply_FreshProject(t *testing.T) {
	h, _ := newHandler(t)

	rr := doApply(t, h, map[string]any{
		"slug": "demo", "name": "Demo",
		"files": []map[string]string{{"name": "build.yaml", "content": sampleFile}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}

	var resp projects.ApplyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.ProjectCreated {
		t.Fatalf("ProjectCreated = false")
	}
	if len(resp.Pipelines) != 1 || resp.Pipelines[0].Name != "build" {
		t.Fatalf("pipelines = %+v", resp.Pipelines)
	}
	if !resp.Pipelines[0].Created || resp.Pipelines[0].MaterialsAdded != 1 {
		t.Fatalf("pipeline state = %+v", resp.Pipelines[0])
	}
}

func TestApply_ReApplyIdempotent(t *testing.T) {
	h, _ := newHandler(t)
	body := map[string]any{
		"slug": "demo", "name": "Demo",
		"files": []map[string]string{{"name": "build.yaml", "content": sampleFile}},
	}
	if rr := doApply(t, h, body); rr.Code != http.StatusOK {
		t.Fatalf("first: %d", rr.Code)
	}
	rr := doApply(t, h, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("second: %d", rr.Code)
	}
	var resp projects.ApplyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.ProjectCreated {
		t.Fatalf("ProjectCreated = true on re-apply")
	}
	if resp.Pipelines[0].Created {
		t.Fatalf("pipeline Created = true on re-apply")
	}
}

func TestApply_InvalidYAML(t *testing.T) {
	h, _ := newHandler(t)
	rr := doApply(t, h, map[string]any{
		"slug": "demo", "name": "Demo",
		"files": []map[string]string{{"name": "bad.yaml", "content": ":\n  :\n    :"}},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "bad.yaml") {
		t.Fatalf("error body missing filename: %s", rr.Body.String())
	}
}

func TestApply_DuplicatePipelineNames(t *testing.T) {
	h, _ := newHandler(t)
	rr := doApply(t, h, map[string]any{
		"slug": "demo", "name": "Demo",
		"files": []map[string]string{
			{"name": "a.yaml", "content": sampleFile},
			{"name": "b.yaml", "content": sampleFile},
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

func TestApply_MissingSlug(t *testing.T) {
	h, _ := newHandler(t)
	rr := doApply(t, h, map[string]any{
		"name":  "Demo",
		"files": []map[string]string{{"name": "build.yaml", "content": sampleFile}},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestApply_WithSCMSourcePersistsAndReturnsID(t *testing.T) {
	h, _ := newHandler(t)

	rr := doApply(t, h, map[string]any{
		"slug": "scm", "name": "SCM",
		"files": []map[string]string{{"name": "build.yaml", "content": sampleFile}},
		"scm_source": map[string]any{
			"provider":        "github",
			"url":             "https://github.com/org/demo",
			"default_branch":  "main",
			"webhook_secret":  "sek",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp projects.ApplyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SCMSource == nil {
		t.Fatalf("scm_source missing from response")
	}
	if !resp.SCMSource.Created || resp.SCMSource.Provider != "github" {
		t.Fatalf("scm_source = %+v", resp.SCMSource)
	}
}

func TestApply_RejectsSCMSourceMissingFields(t *testing.T) {
	h, _ := newHandler(t)
	rr := doApply(t, h, map[string]any{
		"slug": "scm", "name": "SCM",
		"files":      []map[string]string{{"name": "build.yaml", "content": sampleFile}},
		"scm_source": map[string]any{"provider": "github"}, // url missing
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestApply_MethodNotAllowed(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/apply", nil)
	rr := httptest.NewRecorder()
	h.Apply(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rr.Code)
	}
}
