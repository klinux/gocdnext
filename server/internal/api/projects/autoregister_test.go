package projects_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
)

// keyToPEMInline generates a throwaway RSA key as PEM for the App
// client. GitHub never sees the signature because the whole API is
// stubbed, so the key just needs to parse and sign.
func keyToPEMInline(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// fakeGitHubAPI simulates the subset of GitHub API that the App client
// talks to: installation token, list hooks, create hook. Takes
// canned responses so each test can stage its own scenario.
type fakeGitHubAPI struct {
	installationID int64
	installStatus  int // default 200
	listHooks      []map[string]any
	createdPayload atomic.Pointer[map[string]any]
	createdHookID  int64 // default 999
}

func newFakeGitHubAPI() *fakeGitHubAPI {
	return &fakeGitHubAPI{installationID: 100, installStatus: http.StatusOK, createdHookID: 999}
}

func (f *fakeGitHubAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/access_tokens"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "tok",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
		case strings.HasSuffix(r.URL.Path, "/installation"):
			if f.installStatus != http.StatusOK {
				http.Error(w, "not installed", f.installStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": f.installationID})
		case strings.HasSuffix(r.URL.Path, "/hooks") && r.Method == http.MethodGet:
			list := f.listHooks
			if list == nil {
				list = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(list)
		case strings.HasSuffix(r.URL.Path, "/hooks") && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.createdPayload.Store(&body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     f.createdHookID,
				"active": true,
				"events": body["events"],
				"config": body["config"],
			})
		default:
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
	})
}

func applyHandler(t *testing.T, api *fakeGitHubAPI) *projects.Handler {
	t.Helper()
	// newHandler sets up the store cipher — required so the
	// scm_source upsert can seal a generated webhook secret,
	// which auto-register then hands to CreateRepoHook.
	h, _ := newHandler(t)

	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)

	app, err := ghscm.NewAppClient(ghscm.AppConfig{
		AppID:         1,
		PrivateKeyPEM: testPEM(t),
		APIBase:       srv.URL,
	})
	if err != nil {
		t.Fatalf("new app client: %v", err)
	}
	reg := vcs.New()
	reg.Replace(app, []vcs.Integration{{
		Name: "test", Kind: "github_app", Enabled: true, Source: vcs.SourceEnv,
	}})
	return h.WithAutoRegister(projects.AutoRegisterConfig{
		VCS:        reg,
		PublicBase: "https://gocdnext.dev",
	})
}

// testPEM generates a throwaway RSA key for the App client; not
// actually used to sign anything GitHub validates since we stub the
// whole API.
func testPEM(t *testing.T) []byte {
	t.Helper()
	// Use the same helper as the github_test package via a tiny duplicate
	// to avoid cross-package imports of test helpers.
	return keyToPEMInline(t)
}

// applyRequestWithSCMSource builds a minimal apply payload that
// binds an scm_source without any pipeline files. That's the
// exact shape the "Connect repo" UI submits and the shape
// auto-register cares about: it registers one webhook per
// scm_source, not per material. Slug defaults unique per test
// via t.Name so concurrent runs don't collide in the shared DB.
func applyRequestWithSCMSource(slug, url string) map[string]any {
	return map[string]any{
		"slug":  slug,
		"name":  slug,
		"files": []map[string]any{},
		"scm_source": map[string]any{
			"provider":       "github",
			"url":            url,
			"default_branch": "main",
		},
	}
}

func TestAutoRegister_CreatesHookWhenNoneExists(t *testing.T) {
	api := newFakeGitHubAPI()
	h := applyHandler(t, api)

	resp := doApplyRequest(t, h,
		applyRequestWithSCMSource("autoreg-create", "https://github.com/org/repo"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(resp))
	}
	var body projects.ApplyResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if len(body.Webhooks) != 1 {
		t.Fatalf("webhooks = %d", len(body.Webhooks))
	}
	w := body.Webhooks[0]
	if w.Status != "registered" || w.HookID != 999 {
		t.Errorf("webhook = %+v", w)
	}
	if w.SCMSourceURL != "https://github.com/org/repo" {
		t.Errorf("SCMSourceURL = %q", w.SCMSourceURL)
	}

	// Payload check: the hook was POSTed with our public base URL
	// and the plaintext of the freshly-minted scm_source secret —
	// apply returned that in GeneratedWebhookSecret, so the values
	// must match.
	ptr := api.createdPayload.Load()
	if ptr == nil {
		t.Fatal("no POST body captured")
	}
	payload := *ptr
	cfg, _ := payload["config"].(map[string]any)
	if cfg["url"] != "https://gocdnext.dev/api/webhooks/github" {
		t.Errorf("hook url = %v", cfg["url"])
	}
	if body.SCMSource == nil || body.SCMSource.GeneratedWebhookSecret == "" {
		t.Fatalf("expected generated secret in apply response, got %+v", body.SCMSource)
	}
	if cfg["secret"] != body.SCMSource.GeneratedWebhookSecret {
		t.Errorf("hook secret (%v) != apply generated secret (%q)",
			cfg["secret"], body.SCMSource.GeneratedWebhookSecret)
	}
}

func TestAutoRegister_SkipsWhenHookAlreadyExists(t *testing.T) {
	api := newFakeGitHubAPI()
	api.listHooks = []map[string]any{
		{
			"id":     555,
			"active": true,
			"events": []string{"push"},
			"config": map[string]any{
				"url": "https://gocdnext.dev/api/webhooks/github",
			},
		},
	}
	h := applyHandler(t, api)

	resp := doApplyRequest(t, h,
		applyRequestWithSCMSource("autoreg-exists", "https://github.com/org/repo"))
	var body projects.ApplyResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if len(body.Webhooks) != 1 {
		t.Fatalf("webhooks = %d", len(body.Webhooks))
	}
	if body.Webhooks[0].Status != "already_exists" || body.Webhooks[0].HookID != 555 {
		t.Errorf("webhook = %+v", body.Webhooks[0])
	}
	if api.createdPayload.Load() != nil {
		t.Error("create hook should not have been called")
	}
}

func TestAutoRegister_SkipsWhenAppNotInstalled(t *testing.T) {
	api := newFakeGitHubAPI()
	api.installStatus = http.StatusNotFound
	h := applyHandler(t, api)

	resp := doApplyRequest(t, h,
		applyRequestWithSCMSource("autoreg-no-install", "https://github.com/org/repo"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply must still succeed; status = %d", resp.StatusCode)
	}
	var body projects.ApplyResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if len(body.Webhooks) != 1 {
		t.Fatalf("webhooks = %d", len(body.Webhooks))
	}
	if body.Webhooks[0].Status != "skipped_no_install" {
		t.Errorf("status = %q", body.Webhooks[0].Status)
	}
	if api.createdPayload.Load() != nil {
		t.Error("create hook should not have been called")
	}
}

func TestAutoRegister_NoSCMSourceSkipsWebhook(t *testing.T) {
	// Apply without an scm_source — just metadata + pipeline files.
	// There's no repo to register a hook on, so the Webhooks field
	// stays empty.
	api := newFakeGitHubAPI()
	h := applyHandler(t, api)

	req := map[string]any{
		"slug": "plain",
		"name": "plain",
		"files": []map[string]any{{
			"name": "ci.yaml",
			"content": `name: ci
stages: [build]
materials:
  - git:
      url: https://github.com/org/repo
      branch: main
      on: [push]
jobs:
  build:
    stage: build
    script: [go build]
`,
		}},
	}

	resp := doApplyRequest(t, h, req)
	var body projects.ApplyResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if len(body.Webhooks) != 0 {
		t.Errorf("webhooks = %+v, want empty", body.Webhooks)
	}
	if api.createdPayload.Load() != nil {
		t.Error("create hook should not have been called")
	}
}

// Provider != github falls through to "skipped_not_github" —
// GitHubFetcher / the auto-register path only speaks GitHub for
// now. Keeps the hook list clean when a project is bound to a
// placeholder manual provider during dev.
func TestAutoRegister_SkipsNonGitHubProvider(t *testing.T) {
	api := newFakeGitHubAPI()
	h := applyHandler(t, api)

	resp := doApplyRequest(t, h, map[string]any{
		"slug":  "autoreg-manual",
		"name":  "autoreg-manual",
		"files": []map[string]any{},
		"scm_source": map[string]any{
			"provider":       "manual",
			"url":            "internal://repo",
			"default_branch": "main",
		},
	})
	var body projects.ApplyResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if len(body.Webhooks) != 1 {
		t.Fatalf("webhooks = %d", len(body.Webhooks))
	}
	if body.Webhooks[0].Status != "skipped_not_github" {
		t.Errorf("status = %q", body.Webhooks[0].Status)
	}
	if api.createdPayload.Load() != nil {
		t.Error("create hook should not have been called for manual provider")
	}
}

func doApplyRequest(t *testing.T, h *projects.Handler, body any) *http.Response {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/apply", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Apply(rr, req)
	return rr.Result()
}

func readBody(r *http.Response) string {
	b := new(bytes.Buffer)
	_, _ = b.ReadFrom(r.Body)
	return b.String()
}

// context helper for tests not needing it explicitly
func _ctx() context.Context { return context.Background() }
