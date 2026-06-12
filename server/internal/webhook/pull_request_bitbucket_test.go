package webhook_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// newBitbucketServer wires the multi-provider handler with
// HandleBitbucket as the HTTP entry point. Signature scheme is
// HMAC-SHA256 against the shared testSecret — same secret the
// github side uses, but Bitbucket sends it via X-Hub-Signature
// (no "256" suffix in the header name).
func newBitbucketServer(t *testing.T, s *store.Store) http.Handler {
	t.Helper()
	s.SetAuthCipher(newTestCipher(t))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := webhook.NewHandler(s, logger)
	return http.HandlerFunc(h.HandleBitbucket)
}

// postBitbucketPR signs the body with HMAC-SHA256 + testSecret
// and posts it with the canonical headers Bitbucket Cloud
// sends: X-Event-Key (carries the action verb) + X-Hub-Signature
// (hex digest) + X-Request-UUID (delivery id).
func postBitbucketPR(t *testing.T, srv http.Handler, eventKey string, body []byte) *http.Response {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/bitbucket", bytes.NewReader(body))
	req.Header.Set("X-Event-Key", eventKey)
	req.Header.Set("X-Hub-Signature", sig)
	req.Header.Set("X-Request-UUID", "test-pr")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr.Result()
}

func loadBitbucketFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "webhook", "bitbucket", "testdata", name))
	if err != nil {
		t.Fatalf("loadBitbucketFixture %s: %v", name, err)
	}
	return b
}

// seedBitbucketPRMaterial mirrors seedPRMaterial /
// seedGitLabMRMaterial on the bitbucket provider. Clone URL
// matches the destination clone in pr_created.json so
// (url, base_ref) fingerprint resolves the same material row.
func seedBitbucketPRMaterial(t *testing.T, pool *pgxpool.Pool, events []string) uuid.UUID {
	t.Helper()
	url, branch := "https://bitbucket.org/acme/demo.git", "main"
	fp := store.FingerprintFor(url, branch)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))

	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo-bb-pr", Name: "demo bitbucket pr",
		SCMSource: &store.SCMSourceInput{
			Provider: "bitbucket", URL: url, DefaultBranch: branch,
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

// TestBitbucketWebhook_PRCreated_TriggersRun is the end-to-end
// happy path for issue #12 — pullrequest:created webhook lands
// in the dispatch path, fans out to the one matching material
// with `pull_request` in its events list, and persists a run
// carrying cause='pull_request' + the bitbucket-shape
// cause_detail (same keys as github + gitlab, so downstream
// consumers stay provider-uniform).
func TestBitbucketWebhook_PRCreated_TriggersRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newBitbucketServer(t, s)

	_ = seedBitbucketPRMaterial(t, pool, []string{"push", "pull_request"})

	resp := postBitbucketPR(t, srv, "pullrequest:created", loadBitbucketFixture(t, "pr_created.json"))
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
	if len(got.Runs) != 1 || got.PRNumber != 5 {
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
		t.Errorf("revisions branch = %q, want feat/cache-scheduler (source.branch.name)", branch)
	}

	var detail map[string]any
	_ = json.Unmarshal(causeDetail, &detail)
	if v := detail["pr_number"]; v != float64(5) {
		t.Errorf("pr_number in cause_detail = %v, want 5", v)
	}
	if detail["pr_head_sha"] != "abc0123456789abc0123456789abc0123456789a" {
		t.Errorf("pr_head_sha = %v", detail["pr_head_sha"])
	}
	if detail["pr_author"] != "operator-user" {
		t.Errorf("pr_author = %v, want operator-user (bitbucket nickname)", detail["pr_author"])
	}
	if detail["pr_action"] != "created" {
		t.Errorf("pr_action = %v, want created (bitbucket verb)", detail["pr_action"])
	}
	// Bitbucket Cloud has no PR label primitive — pr_labels MUST
	// stay absent from cause_detail (omitempty on len==0) so
	// quorum_by_label readers don't see a misleading empty array.
	if _, present := detail["pr_labels"]; present {
		t.Errorf("pr_labels = %v, want absent on bitbucket (no PR label primitive)", detail["pr_labels"])
	}
}

// TestBitbucketWebhook_PRWhenMaterialDoesNotOptIn asserts the
// per-material event filter — a material listing only `push`
// MUST be skipped for PR webhooks. Mirror of the github +
// gitlab opt-in contract.
func TestBitbucketWebhook_PRWhenMaterialDoesNotOptIn(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newBitbucketServer(t, s)

	_ = seedBitbucketPRMaterial(t, pool, []string{"push"})

	resp := postBitbucketPR(t, srv, "pullrequest:created", loadBitbucketFixture(t, "pr_created.json"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

// TestBitbucketWebhook_PRFulfilledDoesNotTrigger covers the
// non-triggerable terminal action — fulfilled (merged) returns
// 204; the push to destination_branch will handle the merge via
// the push event path.
func TestBitbucketWebhook_PRFulfilledDoesNotTrigger(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newBitbucketServer(t, s)

	_ = seedBitbucketPRMaterial(t, pool, []string{"push", "pull_request"})

	resp := postBitbucketPR(t, srv, "pullrequest:fulfilled", loadBitbucketFixture(t, "pr_created.json"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", resp.StatusCode, readBody(t, resp))
	}
}
