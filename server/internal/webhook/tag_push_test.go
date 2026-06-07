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

// seedTagListeningPipeline creates a project with two pipelines on the
// same repo URL: one listens to push only ("ci"), the other listens to
// push + tag ("release"). Used to assert that the tag-push handler
// fires ONLY the tag-listening row.
func seedTagListeningPipeline(t *testing.T, pool *pgxpool.Pool) (ciID, releaseID uuid.UUID) {
	t.Helper()
	url := "https://github.com/gocdnext/gocdnext.git"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "gocdnext-tag-webhook-test",
		Name: "gocdnext tag webhook test",
		SCMSource: &store.SCMSourceInput{
			Provider:      "github",
			URL:           url,
			DefaultBranch: "main",
			WebhookSecret: testSecret,
		},
		Pipelines: []*domain.Pipeline{
			{
				Name:   "ci",
				Stages: []string{"build"},
				Materials: []domain.Material{{
					Type:        domain.MaterialGit,
					Fingerprint: domain.GitFingerprint(url, "main"),
					AutoUpdate:  true,
					Git: &domain.GitMaterial{
						URL: url, Branch: "main",
						Events: []string{"push"},
					},
				}},
				Jobs: []domain.Job{{
					Name: "c", Stage: "build",
					Tasks: []domain.Task{{Script: "true"}},
				}},
			},
			{
				Name:   "release",
				Stages: []string{"build"},
				Materials: []domain.Material{{
					Type:        domain.MaterialGit,
					Fingerprint: domain.GitFingerprint(url, "@tags"),
					AutoUpdate:  true,
					Git: &domain.GitMaterial{
						URL: url, Branch: "main",
						Events: []string{"push", "tag"},
					},
				}},
				Jobs: []domain.Job{{
					Name: "c", Stage: "build",
					Tasks: []domain.Task{{Script: "true"}},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	for _, p := range res.Pipelines {
		switch p.Name {
		case "ci":
			ciID = p.PipelineID
		case "release":
			releaseID = p.PipelineID
		}
	}
	return ciID, releaseID
}

func TestGitHubWebhook_TagPush_FiresOnlyTagListeners(t *testing.T) {
	pool := dbtest.SetupPool(t)
	_, releaseID := seedTagListeningPipeline(t, pool)
	srv := newServer(t, store.New(pool))

	body := loadFixture(t, "push_tag_lightweight.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", resp.StatusCode, readBody(t, resp))
	}
	var got struct {
		Runs []struct {
			PipelineID string `json:"pipeline_id"`
			RunID      string `json:"run_id"`
		} `json:"runs"`
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if got.Tag != "v1.2.3" {
		t.Errorf("response tag = %q, want v1.2.3", got.Tag)
	}
	if len(got.Runs) != 1 {
		t.Fatalf("runs = %d, want 1 (only the tag-listening pipeline)", len(got.Runs))
	}
	if got.Runs[0].PipelineID != releaseID.String() {
		t.Errorf("fired pipeline = %s, want release pipeline %s",
			got.Runs[0].PipelineID, releaseID)
	}
}

func TestGitHubWebhook_TagPush_StampsCauseAndDetail(t *testing.T) {
	pool := dbtest.SetupPool(t)
	_, releaseID := seedTagListeningPipeline(t, pool)
	srv := newServer(t, store.New(pool))

	body := loadFixture(t, "push_tag_lightweight.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", resp.StatusCode, readBody(t, resp))
	}

	// Inspect the run row directly: cause = "tag" and
	// cause_detail carries tag_name + tag_message + tag_sha + tagger.
	var cause string
	var detail []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT cause, cause_detail FROM runs WHERE pipeline_id = $1`, releaseID,
	).Scan(&cause, &detail); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	if cause != "tag" {
		t.Errorf("cause = %q, want tag", cause)
	}
	var parsed map[string]any
	if err := json.Unmarshal(detail, &parsed); err != nil {
		t.Fatalf("cause_detail JSON: %v", err)
	}
	if parsed["tag_name"] != "v1.2.3" {
		t.Errorf("tag_name = %v, want v1.2.3", parsed["tag_name"])
	}
	if parsed["tag_sha"] != "aaaa1111bbbb2222cccc3333dddd4444eeee5555" {
		t.Errorf("tag_sha = %v, want aaaa1111…5555", parsed["tag_sha"])
	}
	if parsed["tagger"] != "release-bot" {
		t.Errorf("tagger = %v, want release-bot", parsed["tagger"])
	}
	if parsed["tag_message"] == "" || parsed["tag_message"] == nil {
		t.Errorf("tag_message empty, want non-empty (head_commit.message)")
	}
}

func TestGitHubWebhook_TagPush_NoMatchingMaterial(t *testing.T) {
	pool := dbtest.SetupPool(t)
	// No seed — empty DB.
	// We still need the scm_source for HMAC verification to pass,
	// but no pipelines (so the URL lookup returns zero materials).
	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))
	if _, err := s.ApplyProject(context.Background(), store.ApplyProjectInput{
		Slug: "empty-tag-test", Name: "empty",
		SCMSource: &store.SCMSourceInput{
			Provider:      "github",
			URL:           "https://github.com/gocdnext/gocdnext.git",
			DefaultBranch: "main",
			WebhookSecret: testSecret,
		},
	}); err != nil {
		t.Fatalf("apply (no pipelines): %v", err)
	}
	srv := newServer(t, s)

	body := loadFixture(t, "push_tag_lightweight.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (no material); body=%s",
			resp.StatusCode, readBody(t, resp))
	}
}

func TestGitHubWebhook_TagPush_NoTagListenerYields204(t *testing.T) {
	pool := dbtest.SetupPool(t)
	url := "https://github.com/gocdnext/gocdnext.git"
	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))
	// Pipeline EXISTS for the repo but doesn't opt into "tag" — the
	// URL lookup matches but the event filter zeroes it out.
	if _, err := s.ApplyProject(context.Background(), store.ApplyProjectInput{
		Slug: "push-only-tag-test", Name: "push only",
		SCMSource: &store.SCMSourceInput{
			Provider:      "github",
			URL:           url,
			DefaultBranch: "main",
			WebhookSecret: testSecret,
		},
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint(url, "main"),
				AutoUpdate:  true,
				Git: &domain.GitMaterial{
					URL: url, Branch: "main",
					Events: []string{"push"}, // push only, no tag
				},
			}},
			Jobs: []domain.Job{{
				Name: "c", Stage: "build",
				Tasks: []domain.Task{{Script: "true"}},
			}},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	srv := newServer(t, s)

	body := loadFixture(t, "push_tag_lightweight.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (no tag-listener); body=%s",
			resp.StatusCode, readBody(t, resp))
	}
}

func TestGitHubWebhook_TagPush_AnnotatedTagWithoutHeadCommit(t *testing.T) {
	// The "annotated tag" payload variant: head_commit is null but
	// the tag SHA still rides on ev.After. The handler should fire
	// the tag-listening pipeline; cause_detail has tag_name + tag_sha
	// but empty tag_message + tagger.
	pool := dbtest.SetupPool(t)
	_, releaseID := seedTagListeningPipeline(t, pool)
	srv := newServer(t, store.New(pool))

	// push_tag.json already has head_commit: null — perfect fixture.
	body := loadFixture(t, "push_tag.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", resp.StatusCode, readBody(t, resp))
	}

	var cause string
	var detail []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT cause, cause_detail FROM runs WHERE pipeline_id = $1`, releaseID,
	).Scan(&cause, &detail); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	if cause != "tag" {
		t.Errorf("cause = %q, want tag", cause)
	}
	var parsed map[string]any
	if err := json.Unmarshal(detail, &parsed); err != nil {
		t.Fatalf("cause_detail JSON: %v", err)
	}
	if parsed["tag_name"] != "v1.2.3" {
		t.Errorf("tag_name = %v, want v1.2.3", parsed["tag_name"])
	}
	// Empty tag_message + tagger are expected (annotated tag had no
	// head_commit). civars.go will SKIP empty fields so the
	// substitution layer stays literal.
	if parsed["tag_message"] != "" {
		t.Errorf("tag_message = %v, want empty on annotated-tag fixture", parsed["tag_message"])
	}
	if parsed["tagger"] != "" {
		t.Errorf("tagger = %v, want empty on annotated-tag fixture", parsed["tagger"])
	}
}
