package projects_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// rulesetsFake stubs the GitHub endpoints the required-checks reconcile touches:
// the install token, the installation lookup, and the rulesets CRUD. rulesetStatus
// controls the POST response code (200/201 = success, 403 = missing admin perm).
type rulesetsFake struct {
	rulesetStatus int // default 201
	postBody      atomic.Pointer[map[string]any]
}

func (f *rulesetsFake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/access_tokens"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "tok", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
		case strings.HasSuffix(r.URL.Path, "/installation"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 100})
		case strings.HasSuffix(r.URL.Path, "/rulesets") && r.Method == http.MethodGet:
			// Adopt-by-name lookup: no existing gocdnext ruleset.
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/rulesets") && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.postBody.Store(&body)
			code := f.rulesetStatus
			if code == 0 {
				code = http.StatusCreated
			}
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "message": "x"})
		case strings.Contains(r.URL.Path, "/rulesets/") && (r.Method == http.MethodPut || r.Method == http.MethodDelete):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242})
		default:
			t.Errorf("unexpected GitHub call: %s %s", r.Method, r.URL.Path)
		}
	}
}

func newRequiredChecksRouter(t *testing.T, fake *rulesetsFake) (http.Handler, *store.Store, *projects.Handler) {
	t.Helper()
	h, s := newHandler(t)
	if fake != nil {
		srv := httptest.NewServer(fake.handler(t))
		t.Cleanup(srv.Close)
		app, err := ghscm.NewAppClient(ghscm.AppConfig{AppID: 1, PrivateKeyPEM: testPEM(t), APIBase: srv.URL})
		if err != nil {
			t.Fatalf("app client: %v", err)
		}
		reg := vcs.New()
		reg.Replace(app, []vcs.Integration{{Name: "test", Kind: "github_app", Enabled: true, Source: vcs.SourceEnv}})
		h = h.WithAutoRegister(projects.AutoRegisterConfig{VCS: reg, PublicBase: "https://gocdnext.dev"})
	}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/required-checks", h.GetRequiredChecks)
	r.Put("/api/v1/projects/{slug}/required-checks", h.SetRequiredChecks)
	r.Put("/api/v1/projects/{slug}/check-reporting", h.SetCheckReporting)
	return r, s, h
}

// seedRCProject applies "demo" with a PR-firing pipeline "build" and a push-only
// "nightly", bound to a GitHub repo so ParseRepoURL resolves.
func seedRCProject(t *testing.T, s *store.Store) {
	t.Helper()
	url := "https://github.com/org/demo"
	fp := store.FingerprintFor(url, "main")
	pr := &domain.Pipeline{
		Name: "build", Stages: []string{"ci"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push", "pull_request"}},
		}},
		Jobs: []domain.Job{{Name: "compile", Stage: "ci", Tasks: []domain.Task{{Script: "make"}}}},
	}
	push := &domain.Pipeline{
		Name: "nightly", Stages: []string{"ci"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
		}},
		Jobs: []domain.Job{{Name: "sweep", Stage: "ci", Tasks: []domain.Task{{Script: "make sweep"}}}},
	}
	if _, err := s.ApplyProject(context.Background(), store.ApplyProjectInput{
		Slug: "demo", Name: "demo",
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: url, DefaultBranch: "main"},
		Pipelines: []*domain.Pipeline{pr, push},
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

const rcPath = "/api/v1/projects/demo/required-checks"

func TestRequiredChecks_AdminOnlyPut(t *testing.T) {
	r, s, _ := newRequiredChecksRouter(t, &rulesetsFake{})
	seedRCProject(t, s)
	if rr := doReqAs(r, http.MethodPut, rcPath, `{"pipelines":["build"]}`, store.RoleMaintainer); rr.Code != http.StatusForbidden {
		t.Fatalf("maintainer PUT = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	// A maintainer can still read.
	if rr := doReqAs(r, http.MethodGet, rcPath, "", store.RoleMaintainer); rr.Code != http.StatusOK {
		t.Fatalf("maintainer GET = %d, want 200", rr.Code)
	}
}

func TestRequiredChecks_RejectsNonPRFiring(t *testing.T) {
	r, s, _ := newRequiredChecksRouter(t, &rulesetsFake{})
	seedRCProject(t, s)
	// "nightly" exists but is push-only → would deadlock the merge.
	rr := doReqAs(r, http.MethodPut, rcPath, `{"pipelines":["nightly"]}`, store.RoleAdmin)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("push-only pipeline PUT = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRequiredChecks_SyncSuccess(t *testing.T) {
	fake := &rulesetsFake{}
	r, s, _ := newRequiredChecksRouter(t, fake)
	seedRCProject(t, s)

	rr := doReqAs(r, http.MethodPut, rcPath, `{"pipelines":["build"]}`, store.RoleAdmin)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin PUT = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Pipelines      []string `json:"pipelines"`
		StatusContexts []string `json:"status_contexts"`
		Sync           struct {
			Status    string `json:"status"`
			RulesetID *int64 `json:"ruleset_id"`
		} `json:"sync"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Sync.Status != "synced" || resp.Sync.RulesetID == nil || *resp.Sync.RulesetID != 4242 {
		t.Fatalf("sync = %+v, want synced ruleset 4242", resp.Sync)
	}
	if len(resp.StatusContexts) != 1 || resp.StatusContexts[0] != "ci/gocdnext/demo/build" {
		t.Fatalf("status contexts = %v", resp.StatusContexts)
	}
	// The ruleset POST required exactly the demo/build context.
	body := fake.postBody.Load()
	if body == nil {
		t.Fatal("no ruleset POST captured")
	}
	rules, _ := (*body)["rules"].([]any)
	params, _ := rules[0].(map[string]any)["parameters"].(map[string]any)
	rscs, _ := params["required_status_checks"].([]any)
	first, _ := rscs[0].(map[string]any)
	if first["context"] != "ci/gocdnext/demo/build" {
		t.Fatalf("ruleset required context = %v", first["context"])
	}

	// Audited.
	page, err := s.ListAuditEvents(context.Background(), store.ListAuditEventsFilter{
		Action: store.AuditActionProjectRequiredChecksSet,
	})
	if err != nil || page.Total == 0 {
		t.Fatalf("no audit event; total=%d err=%v", page.Total, err)
	}
}

func TestRequiredChecks_AdminPermFailureIsActionable(t *testing.T) {
	fake := &rulesetsFake{rulesetStatus: http.StatusForbidden}
	r, s, _ := newRequiredChecksRouter(t, fake)
	seedRCProject(t, s)

	// Config is saved even though the ruleset write is refused (403).
	rr := doReqAs(r, http.MethodPut, rcPath, `{"pipelines":["build"]}`, store.RoleAdmin)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT with GitHub 403 = %d, want 200 (config saved); body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Pipelines []string `json:"pipelines"`
		Sync      struct {
			Status     string `json:"status"`
			NeedsAdmin bool   `json:"needs_admin"`
		} `json:"sync"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Sync.Status != "failed" || !resp.Sync.NeedsAdmin {
		t.Fatalf("sync = %+v, want failed + needs_admin", resp.Sync)
	}
	if len(resp.Pipelines) != 1 || resp.Pipelines[0] != "build" {
		t.Fatalf("config not preserved on sync failure: %v", resp.Pipelines)
	}
}

func TestRequiredChecks_NoSCMSource(t *testing.T) {
	r, s, _ := newRequiredChecksRouter(t, &rulesetsFake{})
	// Project with no SCM source at all.
	if _, err := s.ApplyProject(context.Background(), store.ApplyProjectInput{Slug: "demo", Name: "demo"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := doReqAs(r, http.MethodPut, rcPath, `{"pipelines":[]}`, store.RoleAdmin)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no-SCM PUT = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// A duplicate pipeline must be rejected by shape validation BEFORE any GitHub
// call — never reach the ruleset payload only to fail at the DB write.
func TestRequiredChecks_DuplicateRejectedBeforeSync(t *testing.T) {
	fake := &rulesetsFake{}
	r, s, _ := newRequiredChecksRouter(t, fake)
	seedRCProject(t, s)
	rr := doReqAs(r, http.MethodPut, rcPath, `{"pipelines":["build","build"]}`, store.RoleAdmin)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("duplicate PUT = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if fake.postBody.Load() != nil {
		t.Fatal("GitHub ruleset was written despite invalid input")
	}
}

// Switching a project to check_run-only while required checks are configured
// would strand the ruleset (the commit-status context stops posting), so it's
// refused with 409.
func TestRequiredChecks_ReportingModeGuard(t *testing.T) {
	r, s, _ := newRequiredChecksRouter(t, &rulesetsFake{})
	seedRCProject(t, s)
	if rr := doReqAs(r, http.MethodPut, rcPath, `{"pipelines":["build"]}`, store.RoleAdmin); rr.Code != http.StatusOK {
		t.Fatalf("configure required = %d; body=%s", rr.Code, rr.Body.String())
	}
	rr := doReqAs(r, http.MethodPut,
		"/api/v1/projects/demo/check-reporting", `{"mode":"check_run"}`, store.RoleAdmin)
	if rr.Code != http.StatusConflict {
		t.Fatalf("check_run switch = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

// After an apply removes a required pipeline, the post-apply reconcile drops it
// from the ruleset (here, the last one → the ruleset is torn down and the config
// cleared) so GitHub never keeps requiring a context that can't be posted.
func TestRequiredChecks_PostApplyPrunesRemovedPipeline(t *testing.T) {
	fake := &rulesetsFake{}
	r, s, h := newRequiredChecksRouter(t, fake)
	seedRCProject(t, s)
	if rr := doReqAs(r, http.MethodPut, rcPath, `{"pipelines":["build"]}`, store.RoleAdmin); rr.Code != http.StatusOK {
		t.Fatalf("configure required = %d; body=%s", rr.Code, rr.Body.String())
	}

	// Re-apply WITHOUT "build" (only the push-only nightly remains).
	url := "https://github.com/org/demo"
	nightly := &domain.Pipeline{
		Name: "nightly", Stages: []string{"ci"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: store.FingerprintFor(url, "main"), AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
		}},
		Jobs: []domain.Job{{Name: "sweep", Stage: "ci", Tasks: []domain.Task{{Script: "make sweep"}}}},
	}
	if _, err := s.ApplyProject(context.Background(), store.ApplyProjectInput{
		Slug: "demo", Name: "demo",
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: url, DefaultBranch: "main"},
		Pipelines: []*domain.Pipeline{nightly},
	}); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	h.ReconcileRequiredChecksAfterApply(context.Background(), "demo")

	// The now-empty requirement cleared the config (ruleset torn down).
	got, err := s.GetProjectRequiredChecks(context.Background(), "demo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected required-checks cleared after the pipeline was removed, got %+v", got)
	}
}
