package projects_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/configsync"
	cryptopkg "github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	gh "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// fakeFetcher is a deterministic, in-process impl of configsync.Fetcher
// for the initial-sync tests — spares the suite from hitting github.com
// and lets us assert exactly which ref + configPath the handler requested.
type fakeFetcher struct {
	files []gh.RawFile
	err   error
	calls int
	last  struct {
		scm        store.SCMSource
		ref        string
		configPath string
	}
}

func (f *fakeFetcher) Fetch(_ context.Context, scm store.SCMSource, ref, configPath string) ([]gh.RawFile, error) {
	f.calls++
	f.last.scm = scm
	f.last.ref = ref
	f.last.configPath = configPath
	return f.files, f.err
}

// HeadSHA is required by configsync.Fetcher but isn't exercised
// by the apply tests — they go through Fetch. The trigger-seed
// tests live in api/runs and use their own fake. Returning a
// deterministic stub keeps the type check happy.
func (f *fakeFetcher) HeadSHA(_ context.Context, _ store.SCMSource, _ string) (string, error) {
	return "deadbeef", nil
}

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
	// SCMSource upsert encrypts the webhook secret via the
	// store cipher — tests that don't wire one get 500s from the
	// store's ErrAuthProviderCipherUnset guard.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c, err := cryptopkg.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	s.SetAuthCipher(c)
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

// TestApply_EmptyFilesCreatesProjectShell mirrors the web "Empty"
// and "Connect repo" flows — both ship zero pipeline YAML files
// and expect the project metadata (+ optional scm_source) to
// persist anyway. Regression test for the UI change that dropped
// "files is required" on the handler.
func TestApply_EmptyFilesCreatesProjectShell(t *testing.T) {
	h, _ := newHandler(t)

	rr := doApply(t, h, map[string]any{
		"slug":        "shell-only",
		"name":        "Shell Only",
		"description": "created via the web with no pipelines yet",
		"files":       []map[string]string{}, // empty on purpose
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ProjectID      string `json:"project_id"`
		ProjectCreated bool   `json:"project_created"`
		Pipelines      []any  `json:"pipelines"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ProjectID == "" || !resp.ProjectCreated {
		t.Fatalf("expected fresh project id + created=true, got %+v", resp)
	}
	if len(resp.Pipelines) != 0 {
		t.Fatalf("pipelines = %d, want 0", len(resp.Pipelines))
	}
}

func TestApply_EmptyFilesWithSCMSourceRegistersBoth(t *testing.T) {
	h, _ := newHandler(t)

	rr := doApply(t, h, map[string]any{
		"slug":  "repo-shell",
		"name":  "Repo Shell",
		"files": []map[string]string{},
		"scm_source": map[string]string{
			"provider":       "github",
			"url":            "https://github.com/org/repo",
			"default_branch": "main",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ProjectCreated bool `json:"project_created"`
		SCMSource      *struct {
			URL string `json:"url"`
		} `json:"scm_source"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if !resp.ProjectCreated {
		t.Fatalf("project not created")
	}
	if resp.SCMSource == nil || resp.SCMSource.URL != "https://github.com/org/repo" {
		t.Fatalf("scm_source = %+v", resp.SCMSource)
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

func TestApply_GeneratesWebhookSecretWhenCallerOmitsIt(t *testing.T) {
	h, _ := newHandler(t)

	rr := doApply(t, h, map[string]any{
		"slug":  "auto-secret",
		"name":  "Auto Secret",
		"files": []map[string]string{},
		"scm_source": map[string]string{
			"provider":       "github",
			"url":            "https://github.com/org/auto-secret",
			"default_branch": "main",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp projects.ApplyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SCMSource == nil || !resp.SCMSource.Created {
		t.Fatalf("scm_source = %+v", resp.SCMSource)
	}
	if got := len(resp.SCMSource.GeneratedWebhookSecret); got != 64 {
		t.Fatalf("generated secret len = %d, want 64 (32 random bytes hex)", got)
	}
}

func TestApply_OmitsGeneratedSecretWhenCallerProvidedOne(t *testing.T) {
	h, _ := newHandler(t)

	rr := doApply(t, h, map[string]any{
		"slug":  "provided",
		"name":  "Provided",
		"files": []map[string]string{},
		"scm_source": map[string]string{
			"provider":       "github",
			"url":            "https://github.com/org/provided",
			"default_branch": "main",
			"webhook_secret": "user-supplied",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp projects.ApplyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SCMSource == nil {
		t.Fatalf("scm_source missing")
	}
	// The operator already knows their secret; don't echo it back.
	if resp.SCMSource.GeneratedWebhookSecret != "" {
		t.Fatalf("expected no generated secret when caller provided one, got %q", resp.SCMSource.GeneratedWebhookSecret)
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

// newHandlerWithFetcher returns the standard test handler wired with
// a fake fetcher so the initial-sync path can be exercised without
// reaching github.com. The fetcher is returned so the test can mutate
// its staged response (files/err) and assert its recorded call args.
func newHandlerWithFetcher(t *testing.T) (*projects.Handler, *fakeFetcher) {
	t.Helper()
	h, _ := newHandler(t)
	f := &fakeFetcher{}
	h = h.WithConfigFetcher(f)
	return h, f
}

const samplePipelineYAML = `name: built-from-remote
materials:
  - git:
      url: https://github.com/org/remote-sync
      branch: main
stages: [build]
jobs:
  compile:
    stage: build
    script: [echo hi]
`

func TestApply_InitialSyncFromRemote(t *testing.T) {
	h, fetcher := newHandlerWithFetcher(t)
	fetcher.files = []gh.RawFile{{Name: "pipeline.yaml", Content: samplePipelineYAML}}

	rr := doApply(t, h, map[string]any{
		"slug":  "remote-sync",
		"name":  "Remote Sync",
		"files": []map[string]string{},
		"scm_source": map[string]string{
			"provider":       "github",
			"url":            "https://github.com/org/remote-sync",
			"default_branch": "main",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetcher calls = %d, want 1", fetcher.calls)
	}
	if got := fetcher.last.ref; got != "main" {
		t.Fatalf("fetcher ref = %q, want main", got)
	}
	if got := fetcher.last.configPath; got != "" {
		t.Fatalf("fetcher configPath = %q, want empty (defaults to .gocdnext)", got)
	}

	var resp projects.ApplyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Pipelines) != 1 || resp.Pipelines[0].Name != "built-from-remote" {
		t.Fatalf("pipelines = %+v", resp.Pipelines)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", resp.Warnings)
	}
}

func TestApply_InitialSyncFolderNotFoundWarns(t *testing.T) {
	h, fetcher := newHandlerWithFetcher(t)
	fetcher.err = fmt.Errorf("wrap: %w", configsync.ErrFolderNotFound)

	rr := doApply(t, h, map[string]any{
		"slug":  "no-folder",
		"name":  "No Folder",
		"files": []map[string]string{},
		"scm_source": map[string]string{
			"provider":       "github",
			"url":            "https://github.com/org/no-folder",
			"default_branch": "main",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp projects.ApplyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Pipelines) != 0 {
		t.Fatalf("expected no pipelines, got %+v", resp.Pipelines)
	}
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], ".gocdnext") {
		t.Fatalf("warnings = %+v", resp.Warnings)
	}
}

func TestApply_InitialSyncEmptyFolderWarns(t *testing.T) {
	h, _ := newHandlerWithFetcher(t)
	// default fetcher: no files, no error → "folder exists, has no YAML".

	rr := doApply(t, h, map[string]any{
		"slug":  "empty-folder",
		"name":  "Empty Folder",
		"files": []map[string]string{},
		"scm_source": map[string]string{
			"provider":       "github",
			"url":            "https://github.com/org/empty-folder",
			"default_branch": "main",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp projects.ApplyResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if len(resp.Pipelines) != 0 {
		t.Fatalf("pipelines = %+v", resp.Pipelines)
	}
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "no YAML files") {
		t.Fatalf("warnings = %+v", resp.Warnings)
	}
}

func TestApply_InitialSyncNetworkErrorFails(t *testing.T) {
	h, fetcher := newHandlerWithFetcher(t)
	fetcher.err = fmt.Errorf("connection refused")

	rr := doApply(t, h, map[string]any{
		"slug":  "net-err",
		"name":  "Net Err",
		"files": []map[string]string{},
		"scm_source": map[string]string{
			"provider":       "github",
			"url":            "https://github.com/org/net-err",
			"default_branch": "main",
		},
	})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestApply_InitialSyncSkippedWhenFilesProvided(t *testing.T) {
	h, fetcher := newHandlerWithFetcher(t)
	fetcher.files = []gh.RawFile{{Name: "pipeline.yaml", Content: samplePipelineYAML}}

	// Caller sent local files + scm_source: local wins, no remote fetch.
	rr := doApply(t, h, map[string]any{
		"slug":  "local-wins",
		"name":  "Local Wins",
		"files": []map[string]string{{"name": "build.yaml", "content": sampleFile}},
		"scm_source": map[string]string{
			"provider":       "github",
			"url":            "https://github.com/org/local-wins",
			"default_branch": "main",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fetcher.calls != 0 {
		t.Fatalf("fetcher calls = %d, want 0 (files were provided)", fetcher.calls)
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
