package webhook_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
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
	t.Helper()
	s.SetAuthCipher(newTestCipher(t))
	appID := testAppID
	if _, err := s.UpsertVCSIntegration(context.Background(), store.UpsertVCSIntegrationInput{
		Name:          "test-app",
		Kind:          "github_app",
		DisplayName:   "Test App",
		AppID:         &appID,
		WebhookSecret: testSecret,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("seed vcs integration: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return http.HandlerFunc(webhook.NewHandler(s, logger).HandleGitHubApp)
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
	// Delivery ledger recorded done.
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM github_app_deliveries WHERE delivery_id='d-1'`).Scan(&status); err != nil {
		t.Fatalf("delivery row: %v", err)
	}
	if status != "done" {
		t.Errorf("delivery status = %q, want done", status)
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
