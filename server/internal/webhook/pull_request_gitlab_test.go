package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook"
)

// newGitLabServer wires the multi-provider handler the same way
// newServer does for github, but exposes HandleGitLab as the
// HTTP entry point. Per-repo secret is the shared testSecret —
// the gitlab side compares it constant-time against
// X-Gitlab-Token, not HMAC. Same store + cipher contract as
// newServer so the seeded scm_source decrypts cleanly.
func newGitLabServer(t *testing.T, s *store.Store) http.Handler {
	t.Helper()
	s.SetAuthCipher(newTestCipher(t))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := webhook.NewHandler(s, logger)
	return http.HandlerFunc(h.HandleGitLab)
}

// postGitLabMR posts an MR fixture with the canonical headers
// GitLab actually sends — token + Merge Request Hook event + a
// UUID delivery id. Body is sent verbatim; signature is just the
// token compare, so no HMAC step.
func postGitLabMR(t *testing.T, srv http.Handler, body []byte) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", testSecret)
	req.Header.Set("X-Gitlab-Event-UUID", "test-mr")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr.Result()
}

// loadGitLabFixture reads from server/internal/webhook/gitlab/testdata/
// (mirror of loadFixture for the github side). Keeps the same
// fixture file the parser unit tests run against — drift between
// integration and parser stays caught.
func loadGitLabFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "webhook", "gitlab", "testdata", name))
	if err != nil {
		t.Fatalf("loadGitLabFixture %s: %v", name, err)
	}
	return b
}

// seedGitLabMRMaterial mirrors seedPRMaterial but on the gitlab
// provider — different clone URL ("https://gitlab.example.com/
// group/demo.git" matching the MR fixture) and a distinct
// project slug so multiple tests in the package can co-exist on
// the same pool without unique-violation noise. The events list
// is parameterised so a single helper drives both the opt-in
// happy path and the no-opt-in 204 case.
func seedGitLabMRMaterial(t *testing.T, pool *pgxpool.Pool, events []string) uuid.UUID {
	t.Helper()
	url, branch := "https://gitlab.example.com/group/demo.git", "main"
	fp := store.FingerprintFor(url, branch)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))

	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo-mr", Name: "demo mr",
		SCMSource: &store.SCMSourceInput{
			Provider: "gitlab", URL: url, DefaultBranch: branch,
			WebhookSecret: testSecret,
		},
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: events},
			}},
			Jobs: []domain.Job{{Name: "lint", Stage: "build", Tasks: []domain.Task{{Script: "go vet ./..."}}}},
		}},
	}); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM materials WHERE fingerprint = $1`, fp,
	).Scan(&id); err != nil {
		t.Fatalf("seed material lookup: %v", err)
	}
	return id
}

// TestGitLabWebhook_MROpened_TriggersRun is the end-to-end happy
// path for issue #11 — MR opened webhook lands in the dispatch
// path, fans out to the one matching material with
// `pull_request` in its events list, and persists a run carrying
// cause='pull_request' + the MR-shape cause_detail (same keys as
// the GitHub PR side, so downstream consumers — CI_PULL_REQUEST_*
// env vars, quorum_by_label — stay provider-uniform).
func TestGitLabWebhook_MROpened_TriggersRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newGitLabServer(t, s)

	_ = seedGitLabMRMaterial(t, pool, []string{"push", "pull_request"})

	resp := postGitLabMR(t, srv, loadGitLabFixture(t, "mr_opened.json"))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, readBody(t, resp))
	}

	var got struct {
		Materials int              `json:"materials"`
		Runs      []map[string]any `json:"runs"`
		PRNumber  int              `json:"pr_number"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Runs) != 1 || got.PRNumber != 7 {
		t.Fatalf("unexpected response %+v", got)
	}
	runIDStr, _ := got.Runs[0]["run_id"].(string)
	if runIDStr == "" {
		t.Fatalf("run_id missing in response: %+v", got.Runs[0])
	}

	runID, _ := uuid.Parse(runIDStr)
	var cause, branch string
	var causeDetail []byte
	_ = pool.QueryRow(context.Background(), `
		SELECT r.cause, r.cause_detail::text,
		       (r.revisions->(SELECT id::text FROM materials LIMIT 1)->>'branch')
		FROM runs r WHERE r.id = $1
	`, runID).Scan(&cause, &causeDetail, &branch)
	if cause != "pull_request" {
		t.Errorf("cause = %q, want pull_request (provider-uniform)", cause)
	}
	if branch != "feat/cache-scheduler" {
		t.Errorf("revisions branch = %q, want feat/cache-scheduler (MR source_branch)", branch)
	}

	var detail map[string]any
	_ = json.Unmarshal(causeDetail, &detail)
	if v := detail["pr_number"]; v != float64(7) {
		t.Errorf("pr_number in cause_detail = %v, want 7", v)
	}
	if detail["pr_head_sha"] != "abc0123456789abc0123456789abc0123456789a" {
		t.Errorf("pr_head_sha = %v", detail["pr_head_sha"])
	}
	if detail["pr_author"] != "operator-user" {
		t.Errorf("pr_author = %v, want operator-user (gitlab user.username)", detail["pr_author"])
	}
	if detail["pr_action"] != "open" {
		t.Errorf("pr_action = %v, want open (gitlab action verb, not 'opened')", detail["pr_action"])
	}
	// Same lowercased + deduped contract as the GitHub side —
	// quorum_by_label reads pr_labels from cause_detail and
	// must not see the provider.
	rawLabels, ok := detail["pr_labels"].([]any)
	if !ok {
		t.Fatalf("pr_labels missing or wrong type: %T (%v)", detail["pr_labels"], detail["pr_labels"])
	}
	want := []string{"hotfix", "needs-review"}
	if len(rawLabels) != len(want) {
		t.Fatalf("pr_labels = %v, want %v", rawLabels, want)
	}
	for i, w := range want {
		if rawLabels[i] != w {
			t.Errorf("pr_labels[%d] = %v, want %q", i, rawLabels[i], w)
		}
	}
}

// TestGitLabWebhook_MRWhenMaterialDoesNotOptIn asserts the same
// per-material event filter the GitHub side uses — a material
// that lists only `push` MUST be skipped for MR webhooks. Webhook
// is the provider boundary; everything past it (including the
// `events: [pull_request]` opt-in) is provider-uniform.
func TestGitLabWebhook_MRWhenMaterialDoesNotOptIn(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newGitLabServer(t, s)

	_ = seedGitLabMRMaterial(t, pool, []string{"push"})

	resp := postGitLabMR(t, srv, loadGitLabFixture(t, "mr_opened.json"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

// TestGitLabWebhook_MRClosedDoesNotTrigger covers the non-
// triggerable actions — close emits the webhook but MUST NOT
// fan out, since the merge into target_branch will arrive as a
// push and handle itself. Mirrors the github "closed → 204"
// contract so operators get the same behaviour everywhere.
func TestGitLabWebhook_MRClosedDoesNotTrigger(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newGitLabServer(t, s)

	_ = seedGitLabMRMaterial(t, pool, []string{"push", "pull_request"})

	body := loadGitLabFixture(t, "mr_opened.json")
	mutated := replaceFirst(t, body, `"action": "open"`, `"action": "close"`)

	resp := postGitLabMR(t, srv, mutated)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", resp.StatusCode, readBody(t, resp))
	}
}
