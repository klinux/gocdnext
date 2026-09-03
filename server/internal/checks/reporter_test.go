package checks_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/checks"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// githubStub emulates the minimum GitHub API surface the reporter
// calls: installation lookup, installation token, create check run,
// patch check run. Tests inject behaviour via its fields.
type githubStub struct {
	installStatus int   // default 200
	installID     int64 // default 100
	nextCheckID   int64 // default 555
	createdBody   atomic.Pointer[map[string]any]
	updatedBody   atomic.Pointer[map[string]any]
	statusBody    atomic.Pointer[map[string]any] // last commit status POST
	statusCount   atomic.Int64
	statusStatus  int // HTTP code for the commit-status POST; 0/201 = OK
}

func newStub() *githubStub {
	return &githubStub{installStatus: http.StatusOK, installID: 100, nextCheckID: 555}
}

func (g *githubStub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/access_tokens"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "inst-tok",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
		case strings.HasSuffix(r.URL.Path, "/installation"):
			if g.installStatus != http.StatusOK {
				http.Error(w, "not found", g.installStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": g.installID})
		case strings.HasSuffix(r.URL.Path, "/check-runs") && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.createdBody.Store(&body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":       g.nextCheckID,
				"status":   body["status"],
				"html_url": "https://github.com/org/repo/runs/1",
			})
		case strings.Contains(r.URL.Path, "/check-runs/") && r.Method == http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.updatedBody.Store(&body)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case strings.Contains(r.URL.Path, "/statuses/") && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.statusBody.Store(&body)
			g.statusCount.Add(1)
			if g.statusStatus != 0 && g.statusStatus != http.StatusCreated {
				http.Error(w, "forbidden", g.statusStatus)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 1}`))
		default:
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
}

// seedWebhookRun creates a project/pipeline/material and a webhook-
// caused run, returning the run id. `repoURL` controls whether the
// material is GitHub-shaped (reporter resolves owner/repo from it).
func seedWebhookRun(t *testing.T, pool *pgxpool.Pool, repoURL string, cause string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	s := store.New(pool)

	fp := domain.GitFingerprint(repoURL, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "chk-" + strings.ReplaceAll(cause, "_", "-"),
		Name: "chk",
		Pipelines: []*domain.Pipeline{{
			Name: "ci", Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: repoURL, Branch: "main", Events: []string{"push", "pull_request"}},
			}},
			Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var matID uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&matID)

	var causeDetail []byte
	if cause == "pull_request" {
		causeDetail, _ = json.Marshal(map[string]any{
			"pr_number":   42,
			"pr_head_sha": "9f7c3d2e1b8a5f6c4e0d7a9b1c3d5e7f9a0b2c4d",
		})
	}
	revision := "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1"
	branch := "main"
	if cause == string(domain.CauseMergeGroup) {
		branch = "gh-readonly-queue/main/pr-42-d8f8c1e"
		causeDetail, _ = json.Marshal(map[string]any{
			"mg_head_sha": revision,
			"mg_head_ref": branch,
			"mg_base_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"mg_base_ref": "main",
			// Guard against accidentally reusing PR semantics for merge-group
			// checks: this value must NOT override the queue head SHA.
			"pr_head_sha": "ffffffffffffffffffffffffffffffffffffffff",
		})
	}

	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     applied.Pipelines[0].PipelineID,
		MaterialID:     matID,
		ModificationID: 1,
		Revision:       revision,
		Branch:         branch, Provider: "github", Delivery: "t", TriggeredBy: "system:webhook",
		Cause:       cause,
		CauseDetail: causeDetail,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return res.RunID
}

func newReporter(t *testing.T, pool *pgxpool.Pool, stub *githubStub) *checks.Reporter {
	t.Helper()
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)

	app, err := ghscm.NewAppClient(ghscm.AppConfig{
		AppID:         1,
		PrivateKeyPEM: throwawayPEM(t),
		APIBase:       srv.URL,
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	reg := vcs.New()
	reg.Replace(app, []vcs.Integration{{
		Name: "test", Kind: "github_app", Enabled: true, Source: vcs.SourceEnv,
	}})
	r := checks.NewReporter(store.New(pool), reg, "https://gocdnext.dev",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if r == nil {
		t.Fatal("reporter is nil")
	}
	return r
}

func throwawayPEM(t *testing.T) []byte {
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

// crid derefs a nullable check-run id for assertions (0 = commit_status mode,
// where no GitHub Check Run exists).
func crid(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func TestNewReporter_ReturnsNilWhenDisabled(t *testing.T) {
	if r := checks.NewReporter(nil, nil, "", nil); r != nil {
		t.Error("expected nil reporter when store+app+base all empty")
	}
}

func TestCreateCheck_PushRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))

	if err := r.CreateCheck(context.Background(), runID); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	// Captured payload should target the run's revision as head_sha
	// and name the pipeline.
	b := stub.createdBody.Load()
	if b == nil {
		t.Fatal("no check run was posted")
	}
	body := *b
	if body["head_sha"] != "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1" {
		t.Errorf("head_sha = %v", body["head_sha"])
	}
	if name, _ := body["name"].(string); !strings.Contains(name, "gocdnext") {
		t.Errorf("name = %v, expected gocdnext prefix", body["name"])
	}
	if body["external_id"] != runID.String() {
		t.Errorf("external_id = %v, want %s", body["external_id"], runID)
	}

	// Store should now have a link row so a follow-up Complete can
	// patch the same check.
	link, err := store.New(pool).GetGithubCheckRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetGithubCheckRun: %v", err)
	}
	if crid(link.CheckRunID) != 555 {
		t.Errorf("check_run_id = %d", crid(link.CheckRunID))
	}
	if link.Owner != "org" || link.Repo != "repo" {
		t.Errorf("owner/repo = %s/%s", link.Owner, link.Repo)
	}
}

func TestCreateCheck_PullRequestPrefersPRHeadSHA(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", "pull_request")
	if err := r.CreateCheck(context.Background(), runID); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	body := *stub.createdBody.Load()
	// PR head SHA from cause_detail must win over the material's
	// revision field.
	if body["head_sha"] != "9f7c3d2e1b8a5f6c4e0d7a9b1c3d5e7f9a0b2c4d" {
		t.Errorf("head_sha = %v", body["head_sha"])
	}
}

func TestCreateCheck_MergeGroupUsesQueueHeadSHAAndStableContext(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseMergeGroup))
	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}

	body := *stub.createdBody.Load()
	if body["head_sha"] != "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1" {
		t.Errorf("head_sha = %v, want merge-group head SHA", body["head_sha"])
	}
	if body["head_sha"] == "ffffffffffffffffffffffffffffffffffffffff" {
		t.Error("merge_group must not reuse pr_head_sha from cause_detail")
	}
	status := stub.statusBody.Load()
	if status == nil {
		t.Fatal("pending commit status was not posted")
	}
	if (*status)["context"] != "ci/gocdnext/chk-merge-group/ci" {
		t.Errorf("status context = %v, want ci/gocdnext/chk-merge-group/ci", (*status)["context"])
	}
	link, err := store.New(pool).GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if link.HeadSHA != "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1" {
		t.Errorf("persisted head_sha = %s, want merge-group SHA", link.HeadSHA)
	}
}

func TestCompleteCheck_SuppressesMergeGroupDestroyedCancellation(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()
	s := store.New(pool)

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseMergeGroup))
	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	beforeStatuses := stub.statusCount.Load()
	stub.updatedBody.Store(nil)

	canceled, err := s.CancelMergeGroupRuns(ctx, "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1", "superseded by queue")
	if err != nil {
		t.Fatalf("cancel merge_group: %v", err)
	}
	if len(canceled) != 1 || canceled[0] != runID {
		t.Fatalf("canceled = %v, want [%s]", canceled, runID)
	}
	if err := r.CompleteCheck(ctx, runID, string(domain.StatusCanceled)); err != nil {
		t.Fatalf("CompleteCheck: %v", err)
	}
	if got := stub.updatedBody.Load(); got != nil {
		t.Fatalf("destroyed merge_group cancellation must not PATCH GitHub, got %v", *got)
	}
	if got := stub.statusCount.Load(); got != beforeStatuses {
		t.Fatalf("destroyed merge_group cancellation posted terminal status count %d -> %d", beforeStatuses, got)
	}
	link, err := s.GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if !link.Completed {
		t.Error("suppressed merge_group check should still be marked completed internally")
	}
}

func TestCreateCheck_NonGitHubRepoSkipped(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)

	runID := seedWebhookRun(t, pool, "https://gitlab.com/org/repo", string(domain.CauseWebhook))
	if err := r.CreateCheck(context.Background(), runID); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if stub.createdBody.Load() != nil {
		t.Error("should not have posted a check for a gitlab URL")
	}
	// And no DB row either.
	if _, err := store.New(pool).GetGithubCheckRun(context.Background(), runID); err == nil {
		t.Error("did not expect a check_run link for non-github repo")
	}
}

func TestCreateCheck_AppNotInstalledSkipped(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	stub.installStatus = http.StatusNotFound
	r := newReporter(t, pool, stub)

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	if err := r.CreateCheck(context.Background(), runID); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if stub.createdBody.Load() != nil {
		t.Error("no POST should have happened when App is not installed")
	}
}

func TestCompleteCheck_UpdatesExistingRow(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("create: %v", err)
	}
	// CompleteCheck reads the run's CURRENT status, so the run must be
	// terminal for it to PATCH.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='success', finished_at=NOW() WHERE id=$1`, runID); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	if err := r.CompleteCheck(ctx, runID, string(domain.StatusSuccess)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	up := stub.updatedBody.Load()
	if up == nil {
		t.Fatal("no PATCH body captured")
	}
	body := *up
	if body["status"] != "completed" {
		t.Errorf("status = %v", body["status"])
	}
	if body["conclusion"] != "success" {
		t.Errorf("conclusion = %v", body["conclusion"])
	}
}

func TestCompleteCheck_NoOpWhenNoLink(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)

	// Seed a run but never call CreateCheck — Complete should be a
	// silent no-op so runs without a GitHub App / install don't spam
	// warnings at terminal time.
	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	if err := r.CompleteCheck(context.Background(), runID, string(domain.StatusFailed)); err != nil {
		t.Errorf("no-op should return nil: %v", err)
	}
	if stub.updatedBody.Load() != nil {
		t.Error("no PATCH should have happened without a prior link")
	}
}

func TestCommitStatus_PostedOnCreateAndComplete(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()
	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))

	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	sb := stub.statusBody.Load()
	if sb == nil {
		t.Fatal("no commit status posted on create")
	}
	s := *sb
	if s["state"] != "pending" {
		t.Errorf("create status state = %v, want pending", s["state"])
	}
	// Project-qualified so two projects on the same repo don't collide (P2).
	if c, _ := s["context"].(string); !strings.HasPrefix(c, "ci/gocdnext/chk-webhook/") {
		t.Errorf("context = %v, want ci/gocdnext/<project>/ prefix", s["context"])
	}
	// The whole point: the status row links STRAIGHT to the run page.
	if url, _ := s["target_url"].(string); !strings.Contains(url, "/runs/"+runID.String()) {
		t.Errorf("target_url = %v, want the run page", s["target_url"])
	}

	// CompleteCheck reads the run's CURRENT status, so make it terminal first.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status=$2, finished_at=NOW() WHERE id=$1`, runID, string(domain.StatusSuccess)); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	if err := r.CompleteCheck(ctx, runID, string(domain.StatusSuccess)); err != nil {
		t.Fatalf("CompleteCheck: %v", err)
	}
	if s = *stub.statusBody.Load(); s["state"] != "success" {
		t.Errorf("complete status state = %v, want success", s["state"])
	}
}

// TestCommitStatus_TerminalUsesPersistedContext pins P1: the terminal update
// reuses the context STORED on the link, not one re-derived at completion (a
// changed/removed material could otherwise leave the status stuck in pending).
func TestCommitStatus_TerminalUsesPersistedContext(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()
	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	// Sentinel context on the link — completion must post with THIS exact value,
	// proving it doesn't re-derive from the (possibly changed) material.
	if _, err := pool.Exec(ctx,
		`UPDATE github_check_runs SET status_context='ci/gocdnext/sentinel/x' WHERE run_id=$1`, runID); err != nil {
		t.Fatalf("set context: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='success', finished_at=NOW() WHERE id=$1`, runID); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	if err := r.CompleteCheck(ctx, runID, string(domain.StatusSuccess)); err != nil {
		t.Fatalf("CompleteCheck: %v", err)
	}
	s := *stub.statusBody.Load()
	if s["context"] != "ci/gocdnext/sentinel/x" {
		t.Errorf("terminal context = %v, want the persisted ci/gocdnext/sentinel/x", s["context"])
	}
	if s["state"] != "success" {
		t.Errorf("terminal state = %v, want success", s["state"])
	}
}

func TestCompleteCheck_StatusMapping(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{string(domain.StatusSuccess), "success"},
		{string(domain.StatusFailed), "failure"},
		{string(domain.StatusCanceled), "cancelled"},
		{string(domain.StatusSkipped), "neutral"},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			pool := dbtest.SetupPool(t)
			stub := newStub()
			r := newReporter(t, pool, stub)
			ctx := context.Background()
			runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
			if err := r.CreateCheck(ctx, runID); err != nil {
				t.Fatalf("create: %v", err)
			}
			// CompleteCheck derives the conclusion from the run's current
			// status, so set it to the case under test.
			if _, err := pool.Exec(ctx,
				`UPDATE runs SET status=$2, finished_at=NOW() WHERE id=$1`, runID, tt.status); err != nil {
				t.Fatalf("finish run: %v", err)
			}
			if err := r.CompleteCheck(ctx, runID, tt.status); err != nil {
				t.Fatalf("complete: %v", err)
			}
			body := *stub.updatedBody.Load()
			if body["conclusion"] != tt.want {
				t.Errorf("conclusion = %v, want %v", body["conclusion"], tt.want)
			}
		})
	}
}

// A rerun re-opens the SAME check run (PATCH → in_progress, no
// conclusion) and reuses the link — no new check run, no churn. This is
// the multi-job-rerun consistency the review flagged: two reruns on one
// run must not orphan check runs.
func TestReopenCheck_ReusesExistingCheckInProgress(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A rerun just put the run back to running (non-terminal).
	if _, err := pool.Exec(ctx, `UPDATE runs SET status='running' WHERE id=$1`, runID); err != nil {
		t.Fatalf("set running: %v", err)
	}
	before, err := store.New(pool).GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link before: %v", err)
	}

	if err := r.ReopenCheck(ctx, runID); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	body := *stub.updatedBody.Load()
	if body["status"] != "in_progress" {
		t.Errorf("status = %v, want in_progress", body["status"])
	}
	if _, ok := body["conclusion"]; ok {
		t.Errorf("re-open must not set a conclusion, got %v", body["conclusion"])
	}
	after, err := store.New(pool).GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link after: %v", err)
	}
	if crid(after.CheckRunID) != crid(before.CheckRunID) {
		t.Errorf("check_run_id changed %d -> %d (re-open must reuse, not recreate)",
			crid(before.CheckRunID), crid(after.CheckRunID))
	}
}

// Once a check has COMPLETED, GitHub won't cleanly reopen it (completed_at is
// set-once), so a rerun must CREATE a fresh check run rather than PATCH the
// stale one back to in_progress. This is the fix for "a rerun only reports the
// result at the end, never that it's running again".
func TestReopenCheck_RecreatesWhenPriorCheckCompleted(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The run finished and the check was completed — completeCheckLocked
	// flips the link's completed flag.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='failed', finished_at=NOW() WHERE id=$1`, runID); err != nil {
		t.Fatalf("fail run: %v", err)
	}
	if err := r.CompleteCheck(ctx, runID, string(domain.StatusFailed)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	before, err := store.New(pool).GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link before: %v", err)
	}
	if !before.Completed {
		t.Fatal("precondition: CompleteCheck must mark the link completed")
	}

	// User reruns: run back to running, and watch for a fresh create vs a
	// reuse PATCH.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='running', finished_at=NULL WHERE id=$1`, runID); err != nil {
		t.Fatalf("set running: %v", err)
	}
	stub.createdBody.Store(nil)
	stub.updatedBody.Store(nil)
	// Hand the recreate a distinct id so we can prove the link is
	// re-pointed at the NEW check run, not just that a POST happened.
	stub.nextCheckID = 777

	if err := r.ReopenCheck(ctx, runID); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// A completed check must be RECREATED (POST), not reused (PATCH).
	if stub.createdBody.Load() == nil {
		t.Error("rerun of a completed check must create a fresh check run (no POST seen)")
	}
	if stub.updatedBody.Load() != nil {
		t.Errorf("must not PATCH a completed check back to in_progress, got %v",
			*stub.updatedBody.Load())
	}
	after, err := store.New(pool).GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link after: %v", err)
	}
	if crid(after.CheckRunID) != 777 || crid(after.CheckRunID) == crid(before.CheckRunID) {
		t.Errorf("link must re-point to the new check run: before=%d after=%d (want 777)",
			crid(before.CheckRunID), crid(after.CheckRunID))
	}
	if after.Completed {
		t.Error("recreated check must reset completed=false")
	}
}

// The fire-and-forget reopen races ReportRunCompleted. When the rerun
// reaches terminal before/while we re-open, the self-heal must close the
// check instead of leaving GitHub hung at in_progress. (The handler spy
// test can't see this — the spy is synchronous.)
func TestReopenCheck_SelfHealsWhenRunAlreadyTerminal(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The rerun finished (failed) before the async reopen landed.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='failed', finished_at=NOW() WHERE id=$1`, runID); err != nil {
		t.Fatalf("fail run: %v", err)
	}

	if err := r.ReopenCheck(ctx, runID); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// Last PATCH must be the self-heal completion, not the in_progress
	// re-open — otherwise the check hangs while the run is terminal.
	body := *stub.updatedBody.Load()
	if body["status"] != "completed" {
		t.Errorf("status = %v, want completed (self-heal)", body["status"])
	}
	if body["conclusion"] != "failure" {
		t.Errorf("conclusion = %v, want failure", body["conclusion"])
	}
}

// The inverse race: the original run's terminal fires async, then the user
// re-runs a job (re-opening the same check). The late, STALE completion
// must not re-close the live rerun — CompleteCheck re-reads and no-ops
// while the run is running.
func TestCompleteCheck_SkipsStaleCompletionWhenRunReopened(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()

	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A rerun is in flight: run back to running, check already re-opened.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='running', finished_at=NULL WHERE id=$1`, runID); err != nil {
		t.Fatalf("set running: %v", err)
	}
	stub.updatedBody.Store(nil) // watch only for an (unwanted) stale PATCH

	// The original run's terminal lands late with a stale 'failed'.
	if err := r.CompleteCheck(ctx, runID, string(domain.StatusFailed)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := stub.updatedBody.Load(); got != nil {
		t.Errorf("stale completion patched a re-opened run's check: %v", *got)
	}
}

// setProjectMode flips the (single) seeded project's check_reporting_mode.
func setProjectMode(t *testing.T, pool *pgxpool.Pool, mode string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE projects SET check_reporting_mode = $1`, mode); err != nil {
		t.Fatalf("set project mode: %v", err)
	}
}

// check_run mode posts ONLY the rich Check Run — no commit status.
func TestCreateCheck_CheckRunMode_SkipsCommitStatus(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()
	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	setProjectMode(t, pool, store.CheckReportingCheckRun)

	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if stub.createdBody.Load() == nil {
		t.Error("check_run mode must still POST the Check Run")
	}
	if stub.statusCount.Load() != 0 {
		t.Errorf("check_run mode must NOT post a commit status, got %d", stub.statusCount.Load())
	}
	link, err := store.New(pool).GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if crid(link.CheckRunID) != 555 {
		t.Errorf("check_run_id = %d, want a real id", crid(link.CheckRunID))
	}
	if link.ReportingMode != store.CheckReportingCheckRun {
		t.Errorf("persisted mode = %q, want check_run", link.ReportingMode)
	}
}

// commit_status mode posts ONLY the straight-to-run Commit Status — no Check
// Run — but STILL persists the identity row (with a NULL check_run_id) so the
// terminal transition can land.
func TestCreateCheck_CommitStatusMode_SkipsCheckRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()
	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	setProjectMode(t, pool, store.CheckReportingCommitStatus)

	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if stub.createdBody.Load() != nil {
		t.Error("commit_status mode must NOT POST a Check Run")
	}
	sb := stub.statusBody.Load()
	if sb == nil {
		t.Fatal("commit_status mode must post the commit status")
	}
	if (*sb)["state"] != "pending" {
		t.Errorf("create status state = %v, want pending", (*sb)["state"])
	}
	link, err := store.New(pool).GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("identity row must persist even without a Check Run: %v", err)
	}
	if link.CheckRunID != nil {
		t.Errorf("check_run_id = %d, want NULL in commit_status mode", crid(link.CheckRunID))
	}
	if link.ReportingMode != store.CheckReportingCommitStatus {
		t.Errorf("persisted mode = %q, want commit_status", link.ReportingMode)
	}
	if link.StatusContext == "" {
		t.Error("status_context must persist so the terminal transition reuses it")
	}
}

// The regression the review flagged: in commit_status mode the terminal
// transition must still post (pending → success) AND flip completed=true —
// NOT get stuck at the initial pending because there's no Check Run.
func TestCommitStatusMode_TerminalTransitionAndCompletedFlag(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()
	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	setProjectMode(t, pool, store.CheckReportingCommitStatus)

	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='success', finished_at=NOW() WHERE id=$1`, runID); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	if err := r.CompleteCheck(ctx, runID, string(domain.StatusSuccess)); err != nil {
		t.Fatalf("CompleteCheck: %v", err)
	}
	// The terminal commit status must have posted success (not stuck pending).
	sb := stub.statusBody.Load()
	if sb == nil || (*sb)["state"] != "success" {
		t.Errorf("terminal commit status state = %v, want success", sb)
	}
	// No Check Run PATCH must have happened (there is no check run).
	if stub.updatedBody.Load() != nil {
		t.Errorf("commit_status mode must not PATCH a Check Run, got %v", *stub.updatedBody.Load())
	}
	// completed lifecycle must be defined even without a Check Run.
	link, err := store.New(pool).GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if !link.Completed {
		t.Error("commit_status terminal must flip completed=true")
	}
}

// In commit_status mode the commit status is the ONLY channel, so a failed
// terminal post (e.g. 403 for a missing "Commit statuses: write" scope) must be
// LOUD — CompleteCheck returns an error and the link is NOT marked completed,
// so a later refresh can retry instead of the run reading "completed" with a
// stuck/absent status. (In both/check_run the Check Run is authoritative and a
// status failure stays best-effort — covered by the other tests.)
func TestCommitStatusMode_TerminalStatusFailureIsLoud(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()
	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	setProjectMode(t, pool, store.CheckReportingCommitStatus)

	// The initial pending status posts fine.
	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Now GitHub rejects the status POST.
	stub.statusStatus = http.StatusForbidden
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='success', finished_at=NOW() WHERE id=$1`, runID); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	if err := r.CompleteCheck(ctx, runID, string(domain.StatusSuccess)); err == nil {
		t.Error("commit_status terminal status failure must return an error, not be swallowed")
	}
	link, err := store.New(pool).GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if link.Completed {
		t.Error("must NOT mark completed when the only channel (commit status) failed to post")
	}
}

// WATCHPOINT: a job rerun on the SAME run after a settings flip must reopen in
// the mode the run STARTED in (persisted on the row), not the project's current
// mode. Start commit_status → complete → flip project to check_run → reopen: the
// recreate must stay commit_status (no Check Run POST, id still NULL).
func TestReopenCheck_UsesPersistedModeNotCurrentProject(t *testing.T) {
	pool := dbtest.SetupPool(t)
	stub := newStub()
	r := newReporter(t, pool, stub)
	ctx := context.Background()
	runID := seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	setProjectMode(t, pool, store.CheckReportingCommitStatus)

	if err := r.CreateCheck(ctx, runID); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='failed', finished_at=NOW() WHERE id=$1`, runID); err != nil {
		t.Fatalf("fail run: %v", err)
	}
	if err := r.CompleteCheck(ctx, runID, string(domain.StatusFailed)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Admin flips the project to check_run AFTER the run started + completed.
	setProjectMode(t, pool, store.CheckReportingCheckRun)

	// Rerun the same run.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='running', finished_at=NULL WHERE id=$1`, runID); err != nil {
		t.Fatalf("set running: %v", err)
	}
	stub.createdBody.Store(nil)
	stub.updatedBody.Store(nil)

	if err := r.ReopenCheck(ctx, runID); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	// Sticky commit_status → the reopen must NOT create a Check Run.
	if stub.createdBody.Load() != nil {
		t.Error("reopen used the project's CURRENT mode (check_run) instead of the run's persisted commit_status")
	}
	link, err := store.New(pool).GetGithubCheckRun(ctx, runID)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if link.CheckRunID != nil {
		t.Errorf("check_run_id = %d, want still NULL (sticky commit_status)", crid(link.CheckRunID))
	}
	if link.ReportingMode != store.CheckReportingCommitStatus {
		t.Errorf("persisted mode drifted to %q, want commit_status", link.ReportingMode)
	}
}

// Store round-trip + the DB CHECK constraint (defense in depth) rejects an
// invalid mode even if a caller bypasses the API-edge validation.
func TestProjectCheckReporting_StoreRoundTripAndDBCheck(t *testing.T) {
	pool := dbtest.SetupPool(t)
	ctx := context.Background()
	s := store.New(pool)
	// seedWebhookRun creates a project (slug chk-webhook) — reuse it.
	seedWebhookRun(t, pool, "https://github.com/org/repo", string(domain.CauseWebhook))
	const slug = "chk-webhook"

	// Default is 'both'.
	if mode, err := s.GetProjectCheckReportingBySlug(ctx, slug); err != nil || mode != store.CheckReportingBoth {
		t.Fatalf("default mode = %q, err=%v; want both", mode, err)
	}
	// Round-trip a valid value.
	if err := s.SetProjectCheckReportingBySlug(ctx, slug, store.CheckReportingCommitStatus); err != nil {
		t.Fatalf("set valid: %v", err)
	}
	if mode, _ := s.GetProjectCheckReportingBySlug(ctx, slug); mode != store.CheckReportingCommitStatus {
		t.Errorf("round-trip mode = %q, want commit_status", mode)
	}
	// The DB CHECK constraint rejects a bogus value (defense in depth).
	if err := s.SetProjectCheckReportingBySlug(ctx, slug, "bogus"); err == nil {
		t.Error("DB CHECK must reject an invalid check_reporting_mode")
	}
	// Unknown project → ErrProjectNotFound, not an opaque error.
	if err := s.SetProjectCheckReportingBySlug(ctx, "no-such-project", store.CheckReportingBoth); !errors.Is(err, store.ErrProjectNotFound) {
		t.Errorf("set on missing project = %v, want ErrProjectNotFound", err)
	}
}
