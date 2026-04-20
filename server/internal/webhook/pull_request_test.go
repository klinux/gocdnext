package webhook_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedPRMaterial creates a pipeline with a git material that lists
// both push and pull_request in its events. Base branch "main"
// matches the PR fixture.
func seedPRMaterial(t *testing.T, pool *pgxpool.Pool, events []string) uuid.UUID {
	t.Helper()
	url, branch := "https://github.com/org/demo.git", "main"
	fp := store.FingerprintFor(url, branch)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := store.New(pool)

	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo-pr", Name: "demo pr",
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

func TestGitHubWebhook_PROpened_TriggersRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)

	_ = seedPRMaterial(t, pool, []string{"push", "pull_request"})

	resp := postSigned(t, srv, "pull_request", loadFixture(t, "pr_opened.json"))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, readBody(t, resp))
	}

	var got struct {
		ModificationID int64  `json:"modification_id"`
		Created        bool   `json:"created"`
		RunID          string `json:"run_id"`
		RunCounter     int64  `json:"run_counter"`
		PRNumber       int    `json:"pr_number"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RunID == "" || got.RunCounter != 1 || got.PRNumber != 42 || !got.Created {
		t.Fatalf("unexpected response %+v", got)
	}

	// Run row must carry cause='pull_request' + the PR metadata in
	// cause_detail; branch must be the PR head ref.
	runID, _ := uuid.Parse(got.RunID)
	var cause, branch string
	var causeDetail []byte
	_ = pool.QueryRow(context.Background(), `
		SELECT r.cause, r.cause_detail::text,
		       (r.revisions->(SELECT id::text FROM materials LIMIT 1)->>'branch')
		FROM runs r WHERE r.id = $1
	`, runID).Scan(&cause, &causeDetail, &branch)
	if cause != "pull_request" {
		t.Errorf("cause = %q, want pull_request", cause)
	}
	if branch != "feat/gizmo" {
		t.Errorf("revisions branch = %q, want feat/gizmo", branch)
	}
	var detail map[string]any
	_ = json.Unmarshal(causeDetail, &detail)
	if fmt := detail["pr_number"]; fmt != float64(42) {
		t.Errorf("pr_number in cause_detail = %v", fmt)
	}
	if detail["pr_head_sha"] != "9f7c3d2e1b8a5f6c4e0d7a9b1c3d5e7f9a0b2c4d" {
		t.Errorf("pr_head_sha missing or wrong: %v", detail["pr_head_sha"])
	}
	if detail["pr_author"] != "kleber" {
		t.Errorf("pr_author = %v", detail["pr_author"])
	}
}

func TestGitHubWebhook_PRWhenMaterialDoesNotOptIn(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)

	// Material listens only for push.
	_ = seedPRMaterial(t, pool, []string{"push"})

	resp := postSigned(t, srv, "pull_request", loadFixture(t, "pr_opened.json"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

func TestGitHubWebhook_PRClosedDoesNotTrigger(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)

	_ = seedPRMaterial(t, pool, []string{"push", "pull_request"})

	// Mutate the fixture to action=closed; expect 204.
	body := loadFixture(t, "pr_opened.json")
	mutated := replaceFirst(t, body, `"action": "opened"`, `"action": "closed"`)

	resp := postSigned(t, srv, "pull_request", mutated)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func replaceFirst(t *testing.T, body []byte, old, new string) []byte {
	t.Helper()
	s := string(body)
	i := indexOf(s, old)
	if i < 0 {
		t.Fatalf("fixture missing %q", old)
	}
	return []byte(s[:i] + new + s[i+len(old):])
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
