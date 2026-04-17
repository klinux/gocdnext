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

	var got struct {
		ModificationID int64 `json:"modification_id"`
		Created        bool  `json:"created"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ModificationID == 0 {
		t.Fatalf("modification_id = 0")
	}
	if !got.Created {
		t.Fatalf("created = false on first call")
	}

	// replay should dedupe (Created=false, same id)
	resp2 := postSigned(t, srv, "push", body)
	defer resp2.Body.Close()
	var got2 struct {
		ModificationID int64 `json:"modification_id"`
		Created        bool  `json:"created"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&got2)
	if got2.Created {
		t.Fatalf("replay Created = true")
	}
	if got2.ModificationID != got.ModificationID {
		t.Fatalf("replay id = %d, want %d", got2.ModificationID, got.ModificationID)
	}

	_ = materialID // seeded, not asserted directly
}

func TestGitHubWebhook_PushNoMatchingMaterial(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)

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

func TestGitHubWebhook_PingEvent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)

	body := []byte(`{"zen":"hello"}`)
	resp := postSigned(t, srv, "ping", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for ping", resp.StatusCode)
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

// --- helpers ---

func newServer(t *testing.T, s *store.Store) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := webhook.NewHandler(testSecret, s, logger)
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

func seedMaterial(t *testing.T, pool *pgxpool.Pool, fingerprint string) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var projectID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (slug, name) VALUES ($1, $2) RETURNING id`,
		"test-"+fingerprint[:8], "test project",
	).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	var pipelineID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO pipelines (project_id, name, definition) VALUES ($1, $2, $3) RETURNING id`,
		projectID, "test-pipeline", []byte(`{}`),
	).Scan(&pipelineID); err != nil {
		t.Fatalf("seed pipeline: %v", err)
	}

	var materialID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO materials (pipeline_id, type, config, fingerprint)
		 VALUES ($1, 'git', $2, $3) RETURNING id`,
		pipelineID, []byte(`{"url":"https://github.com/x/y.git","branch":"main"}`), fingerprint,
	).Scan(&materialID); err != nil {
		t.Fatalf("seed material: %v", err)
	}
	return materialID
}
