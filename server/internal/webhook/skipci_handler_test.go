package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// withHeadCommitMessage rewrites head_commit.message in a push
// fixture so each test controls the marker without a fixture per
// spelling.
func withHeadCommitMessage(t *testing.T, body []byte, msg string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	hc, ok := m["head_commit"].(map[string]any)
	if !ok {
		t.Fatalf("fixture has no head_commit object")
	}
	hc["message"] = msg
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return out
}

func lastDeliveryStatus(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var status string
	err := pool.QueryRow(context.Background(),
		`SELECT status FROM webhook_deliveries ORDER BY id DESC LIMIT 1`,
	).Scan(&status)
	if err != nil {
		t.Fatalf("query last delivery: %v", err)
	}
	return status
}

func TestGitHubWebhook_PushSkipCI(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)

	fp := store.FingerprintFor("https://github.com/gocdnext/gocdnext.git", "main")
	_ = seedMaterial(t, pool, fp)

	// Marked push: acknowledged 200, no run, delivery status "skipped".
	marked := withHeadCommitMessage(t, loadFixture(t, "push_main.json"),
		"chore(deploy): bump image tags [skip ci]")
	resp := postSigned(t, srv, "push", marked)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	var got struct {
		SkippedBy string `json:"skipped_by"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SkippedBy != "[skip ci]" {
		t.Fatalf("skipped_by = %q, want %q", got.SkippedBy, "[skip ci]")
	}
	if st := lastDeliveryStatus(t, pool); st != store.WebhookStatusSkipped {
		t.Fatalf("delivery status = %q, want %q", st, store.WebhookStatusSkipped)
	}

	var runs int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("runs = %d, want 0 — skip-ci push must not create runs", runs)
	}

	// The skip must not have persisted ANY state for the revision: the
	// same push without the marker (e.g. amended + force-pushed, or a
	// provider redelivery of a different commit) still creates a run.
	resp2 := postSigned(t, srv, "push",
		withHeadCommitMessage(t, loadFixture(t, "push_main.json"), "feat: real change"))
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("unmarked push status = %d, want 202; body=%s",
			resp2.StatusCode, readBody(t, resp2))
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("runs after unmarked push = %d, want 1", runs)
	}
}

func TestGitHubWebhook_TagPushSkipCI(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)
	seedTagListeningPipeline(t, pool)

	// push_tag_lightweight.json carries head_commit (lightweight tag),
	// so the marker on the tagged commit is visible and honored.
	marked := withHeadCommitMessage(t, loadFixture(t, "push_tag_lightweight.json"),
		"release notes regen [ci skip]")
	resp := postSigned(t, srv, "push", marked)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	if st := lastDeliveryStatus(t, pool); st != store.WebhookStatusSkipped {
		t.Fatalf("delivery status = %q, want %q", st, store.WebhookStatusSkipped)
	}
	var runs int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("runs = %d, want 0 — skip-ci tag push must not create runs", runs)
	}
}

// Security regression: [skip ci] must NEVER suppress pull_request
// validation — otherwise any contributor bypasses required checks by
// writing the marker into their own commits/PR text. The PR path does
// not consult the marker at all.
func TestGitHubWebhook_PullRequestIgnoresSkipMarker(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)
	_ = seedPRMaterial(t, pool, []string{"push", "pull_request"})

	body := loadFixture(t, "pr_opened.json")
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal pr fixture: %v", err)
	}
	pr, ok := m["pull_request"].(map[string]any)
	if !ok {
		t.Fatalf("fixture has no pull_request object")
	}
	pr["title"] = "evil change [skip ci]"
	pr["body"] = "[ci skip] [no ci]"
	mutated, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal pr fixture: %v", err)
	}

	resp := postSigned(t, srv, "pull_request", mutated)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (PR must run despite markers); body=%s",
			resp.StatusCode, readBody(t, resp))
	}
	var got struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Runs) != 1 {
		t.Fatalf("runs = %d, want 1 — markers must not suppress PR validation", len(got.Runs))
	}
}

// GitLab branch push with a marker exercises the shared
// persistPush path (Bitbucket rides the identical code).
func TestGitLabWebhook_PushSkipCI(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newGitLabServer(t, s)
	_ = seedGitLabMRMaterial(t, pool, []string{"push"})

	payload := map[string]any{
		"object_kind": "push",
		"ref":         "refs/heads/main",
		"before":      "0000000000000000000000000000000000000001",
		"after":       "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		"repository": map[string]any{
			"git_http_url": "https://gitlab.example.com/group/demo.git",
		},
		"commits": []map[string]any{{
			"id":        "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			"message":   "chore: regenerate docs [no ci]",
			"timestamp": "2026-06-12T10:00:00Z",
			"author":    map[string]any{"name": "dev", "email": "dev@example.com"},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	req.Header.Set("X-Gitlab-Token", testSecret)
	req.Header.Set("X-Gitlab-Event-UUID", "test-skipci")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	resp := rr.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	var got struct {
		SkippedBy string `json:"skipped_by"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SkippedBy != "[no ci]" {
		t.Fatalf("skipped_by = %q, want %q", got.SkippedBy, "[no ci]")
	}
	if st := lastDeliveryStatus(t, pool); st != store.WebhookStatusSkipped {
		t.Fatalf("delivery status = %q, want %q", st, store.WebhookStatusSkipped)
	}
	var runs int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("runs = %d, want 0", runs)
	}
}
