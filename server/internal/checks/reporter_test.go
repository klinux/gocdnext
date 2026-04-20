package checks_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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
	"github.com/gocdnext/gocdnext/server/internal/domain"
	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
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

	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     applied.Pipelines[0].PipelineID,
		MaterialID:     matID,
		ModificationID: 1,
		Revision:       "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1",
		Branch:         "main", Provider: "github", Delivery: "t", TriggeredBy: "system:webhook",
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
	r := checks.NewReporter(store.New(pool), app, "https://gocdnext.dev",
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
	if link.CheckRunID != 555 {
		t.Errorf("check_run_id = %d", link.CheckRunID)
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
