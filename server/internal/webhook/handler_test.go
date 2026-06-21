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

	cryptopkg "github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

const testSecret = "it's-a-secret-to-everybody"

func TestGitHubWebhook_PushWithMatchingMaterial(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)

	fp := store.FingerprintFor("https://github.com/gocdnext/gocdnext.git", "main")
	materialID := seedMaterial(t, pool, fp)

	body := loadFixture(t, "push_main.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", resp.StatusCode, readBody(t, resp))
	}

	// Wire shape after v0.4.20 fan-out: a `runs: [...]` array carrying
	// one entry per pipeline that actually got a run created. The first
	// push creates exactly one run (single matching pipeline here).
	var got struct {
		Materials int              `json:"materials"`
		Runs      []map[string]any `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Materials != 1 {
		t.Fatalf("materials = %d, want 1", got.Materials)
	}
	if len(got.Runs) != 1 {
		t.Fatalf("runs = %d, want 1; body=%+v", len(got.Runs), got)
	}
	firstRunID, _ := got.Runs[0]["run_id"].(string)
	if firstRunID == "" {
		t.Fatalf("first run missing run_id: %+v", got.Runs[0])
	}

	// Replay dedupes via (material_id, revision, branch) uniqueness:
	// the modification is the same row, no NEW run is created, runs[]
	// comes back empty.
	resp2 := postSigned(t, srv, "push", body)
	defer resp2.Body.Close()
	var got2 struct {
		Runs []map[string]any `json:"runs"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&got2)
	if len(got2.Runs) != 0 {
		t.Fatalf("replay should not spawn a new run, got %+v", got2.Runs)
	}

	_ = materialID // seeded, not asserted directly
}

func TestGitHubWebhook_PushTriggersRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)

	fp := store.FingerprintFor("https://github.com/gocdnext/gocdnext.git", "main")
	_ = seedMaterial(t, pool, fp)

	body := loadFixture(t, "push_main.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	var got struct {
		Materials int              `json:"materials"`
		Runs      []map[string]any `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Runs) != 1 {
		t.Fatalf("runs = %+v, want 1 entry", got.Runs)
	}
	runID, _ := got.Runs[0]["run_id"].(string)
	if runID == "" {
		t.Fatalf("run_id missing in response: %+v", got.Runs[0])
	}

	ctx := context.Background()
	var status, cause string
	if err := pool.QueryRow(ctx,
		`SELECT status, cause FROM runs WHERE id = $1`, runID,
	).Scan(&status, &cause); err != nil {
		t.Fatalf("run row: %v", err)
	}
	if status != "queued" || cause != "webhook" {
		t.Fatalf("run status=%s cause=%s", status, cause)
	}

	var stageCount, jobCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM stage_runs WHERE run_id = $1`, runID).Scan(&stageCount)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM job_runs WHERE run_id = $1`, runID).Scan(&jobCount)
	if stageCount != 2 || jobCount != 2 {
		t.Fatalf("stages=%d jobs=%d, want 2/2", stageCount, jobCount)
	}

	// Replay must be idempotent: same delivery, no second run.
	resp2 := postSigned(t, srv, "push", body)
	defer resp2.Body.Close()
	var got2 struct {
		Runs []map[string]any `json:"runs"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&got2)
	if len(got2.Runs) != 0 {
		t.Fatalf("replay spawned a new run: %+v", got2.Runs)
	}

	var runCount int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runs r
		 JOIN pipelines p ON p.id = r.pipeline_id
		 JOIN projects pr ON pr.id = p.project_id
		 WHERE pr.slug = 'gocdnext-webhook-test'`,
	).Scan(&runCount)
	if runCount != 1 {
		t.Fatalf("run count after replay = %d, want 1", runCount)
	}
}

// TestGitHubWebhook_PushFansOutToEveryPipeline is the v0.4.20
// regression cover. The real-world report: a multi-pipeline
// project shares the same git material fingerprint
// (same repo + branch) across all its pipelines. Pre-fix only ONE
// ran per push because FindMaterialByFingerprint was `:one LIMIT 1`.
// Post-fix, every pipeline gets a run.
func TestGitHubWebhook_PushFansOutToEveryPipeline(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)
	ctx := context.Background()

	// seedMaterial registers the project+pipeline+scm_source with
	// the first call. Add two more pipelines + matching materials
	// sharing the same fingerprint to simulate the multi-pipeline
	// shape (ci-core, ci-web, security all on push to main).
	fp := store.FingerprintFor("https://github.com/gocdnext/gocdnext.git", "main")
	_ = seedMaterial(t, pool, fp)

	var projectID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM projects WHERE slug = 'gocdnext-webhook-test'`,
	).Scan(&projectID); err != nil {
		t.Fatalf("lookup project: %v", err)
	}
	for _, name := range []string{"ci-web", "security"} {
		var pipelineID uuid.UUID
		if err := pool.QueryRow(ctx,
			`INSERT INTO pipelines (project_id, name, definition, definition_raw) VALUES ($1, $2, $3, $3) RETURNING id`,
			projectID, name, []byte(`{"name":"`+name+`","stages":["build"],"jobs":[{"name":"compile","stage":"build","script":"true"}]}`),
		).Scan(&pipelineID); err != nil {
			t.Fatalf("seed pipeline %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO materials (pipeline_id, type, config, fingerprint)
			 VALUES ($1, 'git', $2, $3)`,
			pipelineID, []byte(`{"url":"https://github.com/x/y.git","branch":"main"}`), fp,
		); err != nil {
			t.Fatalf("seed material %s: %v", name, err)
		}
	}

	body := loadFixture(t, "push_main.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	var got struct {
		Materials int              `json:"materials"`
		Runs      []map[string]any `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Materials != 3 {
		t.Fatalf("materials = %d, want 3", got.Materials)
	}
	if len(got.Runs) != 3 {
		t.Fatalf("runs = %d, want 3; body=%+v", len(got.Runs), got)
	}

	// One run row per pipeline, all in the same project.
	var runCount int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runs r
		 JOIN pipelines p ON p.id = r.pipeline_id
		 WHERE p.project_id = $1`, projectID,
	).Scan(&runCount)
	if runCount != 3 {
		t.Fatalf("runs in DB = %d, want 3 (one per pipeline)", runCount)
	}

	// Replay must dedupe per-material (the (material, revision, branch)
	// uniqueness still holds), so a second delivery creates 0 new runs.
	resp2 := postSigned(t, srv, "push", body)
	defer resp2.Body.Close()
	var got2 struct {
		Runs []map[string]any `json:"runs"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&got2)
	if len(got2.Runs) != 0 {
		t.Fatalf("replay spawned %d runs, want 0", len(got2.Runs))
	}
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runs r
		 JOIN pipelines p ON p.id = r.pipeline_id
		 WHERE p.project_id = $1`, projectID,
	).Scan(&runCount)
	if runCount != 3 {
		t.Fatalf("runs in DB after replay = %d, want 3 (no new runs)", runCount)
	}
}

func TestGitHubWebhook_PushNoMatchingMaterial(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)
	// Seed an scm_source with our testSecret so HMAC resolves,
	// but no pipeline/material → 204 (accepted, nothing to run).
	seedSCMSourceOnly(t, pool, "https://github.com/gocdnext/gocdnext.git", "main")

	body := loadFixture(t, "push_main.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (no material); body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestGitHubWebhook_InvalidSignature(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)
	// Register the repo so HMAC verification actually runs (vs
	// bouncing with "unknown repo"); the bogus signature then
	// fails verification with the real stored secret.
	seedSCMSourceOnly(t, pool, "https://github.com/gocdnext/gocdnext.git", "main")

	body := loadFixture(t, "push_main.json")
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestGitHubWebhook_PingEvent_OrgScoped400(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)

	// Org-scoped ping has no repository.clone_url — handler can't
	// route it to any scm_source for HMAC lookup, so 400.
	body := []byte(`{"zen":"hello"}`)
	resp := postSigned(t, srv, "ping", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for org-ping without repo", resp.StatusCode)
	}
}

func TestGitHubWebhook_UnparsableBody(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)

	body := []byte(`{not json`)
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGitHubWebhook_MissingEventHeader(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)

	body := loadFixture(t, "push_main.json")
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signBody(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestGitHubWebhook_RecordsDeliveryAudit(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)
	// Register the repo up front so the "bad signature" and
	// "no matching material" cases get past the scm_source
	// lookup and produce the right audit statuses.
	seedSCMSourceOnly(t, pool, "https://github.com/gocdnext/gocdnext.git", "main")

	// 1. Signature-rejected delivery → status=rejected, http_status=401.
	body := loadFixture(t, "push_main.json")
	badReq := httptest.NewRequest(http.MethodPost, "/api/webhooks/github", bytes.NewReader(body))
	badReq.Header.Set("X-GitHub-Event", "push")
	badReq.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	badReq.Header.Set("X-GitHub-Delivery", "audit-rejected")
	badReq.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, badReq)

	// 2. No-material delivery → status=ignored, http_status=204.
	resp := postSigned(t, srv, "push", body)
	_ = resp.Body.Close()

	// 3. Matched push → status=accepted, http_status=202, material_id set.
	// seedMaterial re-upserts the same scm_source (COALESCE keeps
	// the secret), then adds the matching git material.
	fp := store.FingerprintFor("https://github.com/gocdnext/gocdnext.git", "main")
	materialID := seedMaterial(t, pool, fp)
	resp2 := postSigned(t, srv, "push", body)
	_ = resp2.Body.Close()

	ctx := context.Background()
	rows, err := pool.Query(ctx,
		`SELECT status, http_status, material_id, error
		 FROM webhook_deliveries
		 WHERE provider = 'github' AND event = 'push'
		 ORDER BY id ASC`,
	)
	if err != nil {
		t.Fatalf("query deliveries: %v", err)
	}
	defer rows.Close()
	type rec struct {
		status string
		code   int32
		matID  *uuid.UUID
		errMsg *string
	}
	var got []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.status, &r.code, &r.matID, &r.errMsg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Fatalf("deliveries = %d, want 3: %+v", len(got), got)
	}
	if got[0].status != "rejected" || got[0].code != 401 || got[0].errMsg == nil {
		t.Fatalf("rejected row = %+v", got[0])
	}
	if got[1].status != "ignored" || got[1].code != 204 || got[1].matID != nil {
		t.Fatalf("ignored row = %+v", got[1])
	}
	if got[2].status != "accepted" || got[2].code != 202 {
		t.Fatalf("accepted row = %+v", got[2])
	}
	if got[2].matID == nil || *got[2].matID != materialID {
		t.Fatalf("accepted row material_id mismatch: got=%v want=%s", got[2].matID, materialID)
	}
}

// --- helpers ---

func newServer(t *testing.T, s *store.Store) http.Handler {
	t.Helper()
	// Per-repo secrets need the cipher wired up on the store —
	// handler 500s on FindSCMSourceWebhookSecret otherwise.
	s.SetAuthCipher(newTestCipher(t))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := webhook.NewHandler(s, logger)
	return http.HandlerFunc(h.HandleGitHub)
}

func postSigned(t *testing.T, srv http.Handler, event string, body []byte) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signBody(body))
	req.Header.Set("X-GitHub-Delivery", "test-"+event)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr.Result()
}

func signBody(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "webhook", "github", "testdata", name))
	if err != nil {
		t.Fatalf("loadFixture %s: %v", name, err)
	}
	return b
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// seedMaterial builds a minimal 2-stage pipeline via ApplyProject with a git
// material matching (url, "main"), so webhook push_main fixtures can drive the
// run-creation path. Returns the material UUID that the webhook handler will
// look up by fingerprint.
func seedMaterial(t *testing.T, pool *pgxpool.Pool, fingerprint string) uuid.UUID {
	t.Helper()
	url := "https://github.com/gocdnext/gocdnext.git"
	branch := "main"
	// Caller's fingerprint is derived from (url, branch). Sanity check.
	if store.FingerprintFor(url, branch) != fingerprint {
		t.Fatalf("seed fingerprint mismatch: caller=%s vs derived", fingerprint)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Per-repo webhook secrets (UI.10.a) require the store to
	// have a cipher + the scm_source to carry the test secret so
	// HandleGitHub's HMAC lookup resolves. Previously the handler
	// took a global token; now the cipher-backed scm_source is
	// the only path.
	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "gocdnext-webhook-test",
		Name: "gocdnext webhook test",
		SCMSource: &store.SCMSourceInput{
			Provider:      "github",
			URL:           url,
			DefaultBranch: branch,
			WebhookSecret: testSecret,
		},
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"build", "test"},
			Materials: []domain.Material{{
				Type:        domain.MaterialGit,
				Fingerprint: fingerprint,
				AutoUpdate:  true,
				Git:         &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}},
				{Name: "unit", Stage: "test", Tasks: []domain.Task{{Script: "make test"}}, Needs: []string{"compile"}},
			},
		}},
	}); err != nil {
		t.Fatalf("seed apply: %v", err)
	}

	var materialID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM materials WHERE fingerprint = $1`, fingerprint,
	).Scan(&materialID); err != nil {
		t.Fatalf("seed material lookup: %v", err)
	}
	return materialID
}

// newTestCipher returns a deterministic AES-256 cipher for use in
// webhook tests. Every suite that exercises the per-repo secret
// path calls SetAuthCipher with this.
func newTestCipher(t *testing.T) *cryptoCipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c, err := cryptopkg.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

// Type alias via pointer helper so test code reads naturally without
// importing the package at every call site.
type cryptoCipher = cryptopkg.Cipher
