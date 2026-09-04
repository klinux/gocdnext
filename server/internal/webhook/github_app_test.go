package webhook_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/checks"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
	"github.com/gocdnext/gocdnext/server/internal/webhook"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

const (
	testAppID   int64 = 424242
	testInstID  int64 = 100
	testCheckID int64 = 555
	testOwner         = "org"
	testRepo          = "repo"
)

// newAppServer builds the App-webhook handler over a cipher-wired store and
// seeds a github_app integration whose webhook secret is testSecret (so the
// signBody signatures verify).
func newAppServer(t *testing.T, s *store.Store) http.Handler {
	return newAppServerWithReporter(t, s, true)
}

func newAppServerWithoutReporter(t *testing.T, s *store.Store) http.Handler {
	return newAppServerWithReporter(t, s, false)
}

func newAppServerWithReporter(t *testing.T, s *store.Store, withReporter bool) http.Handler {
	t.Helper()
	stub := &appGitHubStub{installID: testInstID, nextCheckID: testCheckID}
	return newAppServerWithReporterStub(t, s, withReporter, stub)
}

func newAppServerWithReporterStub(t *testing.T, s *store.Store, withReporter bool, stub *appGitHubStub) http.Handler {
	t.Helper()
	api := httptest.NewServer(stub.handler(t))
	t.Cleanup(api.Close)
	s.SetAuthCipher(newTestCipher(t))
	appID := testAppID
	if _, err := s.UpsertVCSIntegration(context.Background(), store.UpsertVCSIntegrationInput{
		Name:          "test-app",
		Kind:          "github_app",
		DisplayName:   "Test App",
		AppID:         &appID,
		PrivateKeyPEM: throwawayAppPEM(t),
		WebhookSecret: testSecret,
		APIBase:       api.URL,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("seed vcs integration: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := webhook.NewHandler(s, logger)
	if withReporter {
		reporter := checks.NewReporter(s, vcs.New(), "https://gocdnext.dev", logger)
		h = h.WithChecksReporter(reporter)
	}
	return http.HandlerFunc(h.HandleGitHubApp)
}

type appGitHubStub struct {
	installID   int64
	nextCheckID int64
	checkStatus int
	statusCode  int
	statusPosts atomic.Int64
}

func (g *appGitHubStub) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": g.installID})
		case strings.Contains(r.URL.Path, "/access_tokens") && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "inst-tok",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
		case strings.HasSuffix(r.URL.Path, "/check-runs") && r.Method == http.MethodPost:
			if g.checkStatus != 0 && g.checkStatus != http.StatusCreated {
				http.Error(w, "check failed", g.checkStatus)
				return
			}
			id := g.nextCheckID
			if id == 0 {
				id = testCheckID
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case strings.Contains(r.URL.Path, "/statuses/") && r.Method == http.MethodPost:
			g.statusPosts.Add(1)
			if g.statusCode != 0 && g.statusCode != http.StatusCreated {
				http.Error(w, "status failed", g.statusCode)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		default:
			t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
}

func throwawayAppPEM(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
}

func signBodyWith(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// seedRerunnableRun creates a project/pipeline/material + a modification + a
// TERMINAL run, and links a github_check_runs row so the App re-run cross-check
// passes. Returns the run id. RerunRun resolves the modification by
// (material, revision, branch) — hence the explicit InsertModification.
func seedRerunnableRun(t *testing.T, pool *pgxpool.Pool, s *store.Store, terminal bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	const repoURL = "https://github.com/org/repo"
	const rev = "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1"

	fp := domain.GitFingerprint(repoURL, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "app-rerun", Name: "app-rerun",
		Pipelines: []*domain.Pipeline{{
			Name: "ci", Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: repoURL, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var matID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&matID); err != nil {
		t.Fatalf("material id: %v", err)
	}
	mod, err := s.InsertModification(ctx, store.Modification{MaterialID: matID, Revision: rev, Branch: "main"})
	if err != nil {
		t.Fatalf("insert modification: %v", err)
	}
	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applied.Pipelines[0].PipelineID, MaterialID: matID, ModificationID: mod.ID,
		Revision: rev, Branch: "main", Provider: "github", Delivery: "seed", TriggeredBy: "test",
		Cause: string(domain.CauseWebhook),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	checkID := testCheckID
	if err := s.UpsertGithubCheckRun(ctx, store.UpsertGithubCheckRunInput{
		RunID: res.RunID, InstallationID: testInstID, CheckRunID: &checkID,
		Owner: testOwner, Repo: testRepo, HeadSHA: rev,
		StatusContext: "ci/gocdnext/app-rerun/ci", ReportingMode: store.CheckReportingBoth,
	}); err != nil {
		t.Fatalf("upsert check link: %v", err)
	}
	if terminal {
		if _, err := pool.Exec(ctx,
			`UPDATE runs SET status='success', finished_at=NOW() WHERE id=$1`, res.RunID); err != nil {
			t.Fatalf("finish run: %v", err)
		}
	}
	return res.RunID
}

func checkRunBody(action, externalID string, checkID, appID, instID int64) []byte {
	return []byte(fmt.Sprintf(`{
		"action": %q,
		"check_run": {"id": %d, "external_id": %q, "app": {"id": %d}},
		"installation": {"id": %d},
		"repository": {"name": %q, "owner": {"login": %q}}
	}`, action, checkID, externalID, appID, instID, testRepo, testOwner))
}

func postApp(t *testing.T, srv http.Handler, event, delivery string, body []byte, sig string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/github/app", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr.Result()
}

func runCount(t *testing.T, pool *pgxpool.Pool, slug string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs r JOIN pipelines p ON p.id=r.pipeline_id
		 JOIN projects pr ON pr.id=p.project_id WHERE pr.slug=$1`, slug).Scan(&n); err != nil {
		t.Fatalf("run count: %v", err)
	}
	return n
}

func TestGitHubApp_RerunOnRerequested(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)
	runID := seedRerunnableRun(t, pool, s, true)

	body := checkRunBody("rerequested", runID.String(), testCheckID, testAppID, testInstID)
	resp := postApp(t, srv, "check_run", "d-1", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", resp.StatusCode, readBody(t, resp))
	}
	if n := runCount(t, pool, "app-rerun"); n != 2 {
		t.Errorf("run count = %d, want 2 (a rerun was created)", n)
	}
	// Ledger row exists and is linked to the new run (recorded atomically).
	var linked bool
	if err := pool.QueryRow(context.Background(),
		`SELECT run_id IS NOT NULL FROM github_app_deliveries WHERE delivery_id='d-1'`).Scan(&linked); err != nil {
		t.Fatalf("delivery row: %v", err)
	}
	if !linked {
		t.Error("delivery ledger row must link the created run (run_id set)")
	}
}

func TestGitHubApp_InvalidSignature(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)
	runID := seedRerunnableRun(t, pool, s, true)

	body := checkRunBody("rerequested", runID.String(), testCheckID, testAppID, testInstID)
	resp := postApp(t, srv, "check_run", "d-bad", body, "sha256=deadbeef")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if n := runCount(t, pool, "app-rerun"); n != 1 {
		t.Errorf("run count = %d, want 1 (no rerun on bad signature)", n)
	}
}

func TestGitHubApp_PingAccepted(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)

	body := []byte(fmt.Sprintf(`{"zen":"hi","hook":{"app_id":%d}}`, testAppID))
	resp := postApp(t, srv, "ping", "d-ping", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ping status = %d, want 204; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestGitHubApp_DuplicateDeliveryIsIdempotent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)
	runID := seedRerunnableRun(t, pool, s, true)

	body := checkRunBody("rerequested", runID.String(), testCheckID, testAppID, testInstID)
	sig := signBody(body)
	// Same X-GitHub-Delivery twice → exactly one rerun.
	postApp(t, srv, "check_run", "dup-1", body, sig).Body.Close()
	resp2 := postApp(t, srv, "check_run", "dup-1", body, sig)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("redelivery status = %d, want 204", resp2.StatusCode)
	}
	if n := runCount(t, pool, "app-rerun"); n != 2 {
		t.Errorf("run count = %d, want 2 (redelivery must not create a second rerun)", n)
	}
}

func TestGitHubApp_ActiveRunGuard(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)
	runID := seedRerunnableRun(t, pool, s, false) // left queued (active)

	body := checkRunBody("rerequested", runID.String(), testCheckID, testAppID, testInstID)
	resp := postApp(t, srv, "check_run", "d-active", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if n := runCount(t, pool, "app-rerun"); n != 1 {
		t.Errorf("run count = %d, want 1 (active run must not be rerun)", n)
	}
}

func TestGitHubApp_CrossCheckMismatchIgnored(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)
	runID := seedRerunnableRun(t, pool, s, true)

	// Wrong check_run.id vs the persisted link → ignored, no rerun.
	body := checkRunBody("rerequested", runID.String(), testCheckID+1, testAppID, testInstID)
	resp := postApp(t, srv, "check_run", "d-mismatch", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if n := runCount(t, pool, "app-rerun"); n != 1 {
		t.Errorf("run count = %d, want 1 (identity mismatch must not rerun)", n)
	}
}

// Two concurrent redeliveries of the SAME X-GitHub-Delivery must both return
// 204 and create EXACTLY ONE rerun — the loser blocks on the ledger PK claim
// (at tx start, before the counter/InsertRun) and rolls back, never colliding
// on runs(pipeline_id, counter) or 500ing.
func TestGitHubApp_ConcurrentSameDeliveryOneRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)
	runID := seedRerunnableRun(t, pool, s, true)

	body := checkRunBody("rerequested", runID.String(), testCheckID, testAppID, testInstID)
	sig := signBody(body)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := postApp(t, srv, "check_run", "conc-1", body, sig)
			codes[i] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		if c != http.StatusNoContent {
			t.Errorf("concurrent delivery status = %d, want 204 (idempotent, no 500)", c)
		}
	}
	if n := runCount(t, pool, "app-rerun"); n != 2 {
		t.Errorf("run count = %d, want 2 (exactly one rerun from concurrent duplicates)", n)
	}
}

func checkSuiteBody(action string, appID, instID int64) []byte {
	return []byte(fmt.Sprintf(`{
		"action": %q,
		"check_suite": {"app": {"id": %d}},
		"installation": {"id": %d},
		"repository": {"name": %q, "owner": {"login": %q}}
	}`, action, appID, instID, testRepo, testOwner))
}

// A check_suite event (deferred — no check_run object) must still AUTHENTICATE
// in a multi-App install: its app id lives at check_suite.app.id, so the secret
// resolver can pick the right App instead of failing the sole-App fallback and
// 401ing. Verified → the deferred event is ignored (204), not redelivered.
func TestGitHubApp_CheckSuiteMultiAppAuthenticates(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s) // seeds testAppID with testSecret
	// A SECOND enabled github_app with a different app_id + secret makes the
	// sole-App fallback fail — so a 204 proves check_suite.app.id was used.
	otherID := testAppID + 1
	if _, err := s.UpsertVCSIntegration(context.Background(), store.UpsertVCSIntegrationInput{
		Name: "other-app", Kind: "github_app", AppID: &otherID,
		WebhookSecret: "other-secret", Enabled: true,
	}); err != nil {
		t.Fatalf("seed second app: %v", err)
	}

	body := checkSuiteBody("rerequested", testAppID, testInstID) // our app, signed with testSecret
	resp := postApp(t, srv, "check_suite", "cs-1", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("check_suite status = %d, want 204 (authenticated via check_suite.app.id, then ignored)", resp.StatusCode)
	}
}

func TestGitHubApp_EventWithUnknownAppIDDoesNotTryOtherSecrets(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)

	body := checkSuiteBody("rerequested", testAppID+999, testInstID)
	resp := postApp(t, srv, "check_suite", "cs-unknown", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for unknown app_id even with another valid configured secret; body=%s",
			resp.StatusCode, readBody(t, resp))
	}
}

func TestGitHubApp_MergeGroupMultiAppAuthenticatesWithoutAppID(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s) // seeds testAppID with testSecret
	otherID := testAppID + 1
	if _, err := s.UpsertVCSIntegration(context.Background(), store.UpsertVCSIntegrationInput{
		Name: "other-app", Kind: "github_app", AppID: &otherID,
		WebhookSecret: "other-secret", Enabled: true,
	}); err != nil {
		t.Fatalf("seed second app: %v", err)
	}

	body := []byte(`{
		"action": "checks_requested",
		"installation": {"id": 100},
		"repository": {"full_name": "org/repo", "clone_url": "https://github.com/org/repo.git"},
		"merge_group": {
			"head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"head_ref": "refs/heads/gh-readonly-queue/main/pr-1-aaaa",
			"base_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"base_ref": "refs/heads/main",
			"head_commit": {"id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		}
	}`)
	resp := postApp(t, srv, "merge_group", "mg-auth", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("merge_group status = %d, want 204 (authenticated by trying all App secrets, then no material); body=%s",
			resp.StatusCode, readBody(t, resp))
	}
}

func TestGitHubApp_MergeGroupWithoutAppIDRejectsInvalidSignature(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)

	body := mergeGroupBody("checks_requested")
	resp := postApp(t, srv, "merge_group", "mg-bad-sig", body, "sha256=deadbeef")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for invalid app-less merge_group signature; body=%s",
			resp.StatusCode, readBody(t, resp))
	}
}

func TestGitHubApp_MergeGroupWithoutAppIDRejectsWhenNoAppSecretsConfigured(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := http.HandlerFunc(webhook.NewHandler(s, logger).HandleGitHubApp)

	body := mergeGroupBody("checks_requested")
	resp := postApp(t, srv, "merge_group", "mg-no-app", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when no enabled App secret can authenticate; body=%s",
			resp.StatusCode, readBody(t, resp))
	}
}

func TestGitHubApp_MergeGroupApplessSignatureBindsToMatchedApp(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s) // App A: secret testSecret, installation 100.
	seedMergeGroupPipelines(t, pool)

	// App B has a valid webhook secret too, but resolves org/repo to a different
	// installation. A body signed by B must not be allowed to create runs/checks
	// for App A's installation id just because the payload lacks app.id.
	otherStub := &appGitHubStub{installID: testInstID + 999, nextCheckID: testCheckID + 999}
	otherAPI := httptest.NewServer(otherStub.handler(t))
	t.Cleanup(otherAPI.Close)
	otherID := testAppID + 1
	if _, err := s.UpsertVCSIntegration(context.Background(), store.UpsertVCSIntegrationInput{
		Name:          "other-app",
		Kind:          "github_app",
		AppID:         &otherID,
		PrivateKeyPEM: throwawayAppPEM(t),
		WebhookSecret: "other-secret",
		APIBase:       otherAPI.URL,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("seed second app: %v", err)
	}

	body := mergeGroupBody("checks_requested")
	resp := postApp(t, srv, "merge_group", "mg-wrong-app", body, signBodyWith(body, "other-secret"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when matched App does not own payload installation; body=%s",
			resp.StatusCode, readBody(t, resp))
	}
	if n := runCount(t, pool, "merge-group"); n != 0 {
		t.Fatalf("runs = %d, want no fan-out for wrong authenticated App", n)
	}
}
