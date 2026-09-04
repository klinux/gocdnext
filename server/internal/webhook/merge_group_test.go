package webhook_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

const (
	mgRepoURL  = "https://github.com/org/repo.git"
	mgHeadSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mgHeadRef  = "gh-readonly-queue/main/pr-1-aaaa"
	mgBaseSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	mgBaseRef  = "main"
	mgDelivery = "mg-checks-requested"
)

func seedMergeGroupPipelines(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	s := store.New(pool)
	pipelines := []*domain.Pipeline{
		{
			Name:   "ci",
			Stages: []string{"build"},
			Materials: []domain.Material{
				{
					Type:        domain.MaterialGit,
					Fingerprint: domain.GitFingerprint(mgRepoURL, "main"),
					AutoUpdate:  true,
					Git: &domain.GitMaterial{
						URL: mgRepoURL, Branch: "main",
						Events: []string{"pull_request"},
					},
				},
				{
					Type:        domain.MaterialGit,
					Fingerprint: domain.GitFingerprint(mgRepoURL, "release"),
					AutoUpdate:  true,
					Git: &domain.GitMaterial{
						URL: mgRepoURL, Branch: "release",
						Events: []string{"pull_request"},
					},
				},
			},
			Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
		},
		{
			Name:   "push-only",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint(mgRepoURL, "main"),
				AutoUpdate:  true,
				Git: &domain.GitMaterial{
					URL: mgRepoURL, Branch: "main",
					Events: []string{"push"},
				},
			}},
			Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
		},
		{
			Name:   "path-scoped",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint(mgRepoURL, "main"),
				AutoUpdate:  true,
				Git: &domain.GitMaterial{
					URL: mgRepoURL, Branch: "main",
					Events: []string{"pull_request"},
					Paths:  []string{"docs/**"},
				},
			}},
			Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
		},
	}
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "merge-group", Name: "merge group",
		Pipelines: pipelines,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func mergeGroupBody(action string) []byte {
	reason := ""
	if action == "destroyed" {
		reason = `"reason": "invalidated",`
	}
	return []byte(`{
		"action": "` + action + `",
		` + reason + `
		"installation": {"id": 100},
		"repository": {
			"full_name": "org/repo",
			"clone_url": "` + mgRepoURL + `"
		},
		"merge_group": {
			"head_sha": "` + mgHeadSHA + `",
			"head_ref": "refs/heads/` + mgHeadRef + `",
			"base_sha": "` + mgBaseSHA + `",
			"base_ref": "refs/heads/` + mgBaseRef + `",
			"head_commit": {
				"id": "` + mgHeadSHA + `",
				"message": "Merge queue",
				"timestamp": "2026-08-30T10:00:00Z"
			}
		}
	}`)
}

func TestGitHubAppMergeGroupChecksRequestedDispatchesEligiblePipelines(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)
	seedMergeGroupPipelines(t, pool)

	body := mergeGroupBody("checks_requested")
	resp := postApp(t, srv, "merge_group", mgDelivery, body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", resp.StatusCode, readBody(t, resp))
	}
	var got struct {
		Materials    int              `json:"materials"`
		PathsIgnored int              `json:"paths_ignored"`
		Runs         []map[string]any `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Materials != 2 || len(got.Runs) != 2 || got.PathsIgnored != 1 {
		t.Fatalf("response = %+v, want 2 materials/runs and 1 paths_ignored", got)
	}

	rows, err := pool.Query(context.Background(), `
		SELECT p.name, r.id, r.cause, r.revisions, r.cause_detail
		FROM runs r
		JOIN pipelines p ON p.id = r.pipeline_id
		ORDER BY p.name
	`)
	if err != nil {
		t.Fatalf("runs query: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name, cause string
		var runID uuid.UUID
		var revisions, detail []byte
		if err := rows.Scan(&name, &runID, &cause, &revisions, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[name] = true
		if cause != string(domain.CauseMergeGroup) {
			t.Fatalf("%s cause = %s, want merge_group", name, cause)
		}
		var rev map[string]struct {
			Revision string `json:"revision"`
			Branch   string `json:"branch"`
		}
		if err := json.Unmarshal(revisions, &rev); err != nil {
			t.Fatalf("%s revisions: %v", name, err)
		}
		if len(rev) != 1 {
			t.Fatalf("%s revisions = %+v, want one triggering material", name, rev)
		}
		for _, r := range rev {
			if r.Revision != mgHeadSHA || r.Branch != mgHeadRef {
				t.Fatalf("%s revision = %+v, want head sha + queue ref", name, r)
			}
		}
		var cd struct {
			HeadSHA string `json:"mg_head_sha"`
			HeadRef string `json:"mg_head_ref"`
			BaseSHA string `json:"mg_base_sha"`
			BaseRef string `json:"mg_base_ref"`
		}
		if err := json.Unmarshal(detail, &cd); err != nil {
			t.Fatalf("%s cause_detail: %v", name, err)
		}
		if cd.HeadSHA != mgHeadSHA || cd.HeadRef != mgHeadRef ||
			cd.BaseSHA != mgBaseSHA || cd.BaseRef != mgBaseRef {
			t.Fatalf("%s cause_detail = %+v", name, cd)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !seen["ci"] || !seen["path-scoped"] || seen["push-only"] {
		t.Fatalf("runs by pipeline = %v, want ci + path-scoped only", seen)
	}
}

func TestGitHubAppMergeGroupChecksRequestedDedupesReplay(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)
	seedMergeGroupPipelines(t, pool)

	body := mergeGroupBody("checks_requested")
	resp := postApp(t, srv, "merge_group", mgDelivery, body, signBody(body))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", resp.StatusCode)
	}
	resp = postApp(t, srv, "merge_group", mgDelivery+"-retry", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retry status = %d, want 202; body=%s", resp.StatusCode, readBody(t, resp))
	}

	var runs int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM runs
		WHERE cause = $1
	`, string(domain.CauseMergeGroup)).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 2 {
		t.Fatalf("merge_group runs = %d, want exactly one per eligible pipeline", runs)
	}
}

func TestGitHubAppMergeGroupRequiresReporterBeforeFanout(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServerWithoutReporter(t, s)
	seedMergeGroupPipelines(t, pool)

	body := mergeGroupBody("checks_requested")
	resp := postApp(t, srv, "merge_group", mgDelivery, body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when reporter is disabled; body=%s",
			resp.StatusCode, readBody(t, resp))
	}
	var runs int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs WHERE cause = $1`, string(domain.CauseMergeGroup)).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("runs = %d, want no fan-out without a reporter", runs)
	}
}

func TestGitHubAppMergeGroupReportingErrorReturns500AndRetryRepostsExistingRuns(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	stub := &appGitHubStub{installID: testInstID, nextCheckID: testCheckID, checkStatus: http.StatusInternalServerError}
	srv := newAppServerWithReporterStub(t, s, true, stub)
	seedMergeGroupPipelines(t, pool)

	body := mergeGroupBody("checks_requested")
	resp := postApp(t, srv, "merge_group", mgDelivery+"-report-fail", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when a required check cannot be created; body=%s",
			resp.StatusCode, readBody(t, resp))
	}
	var runs int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM runs
		WHERE cause = $1
	`, string(domain.CauseMergeGroup)).Scan(&runs); err != nil {
		t.Fatalf("count runs after report failure: %v", err)
	}
	if runs != 2 {
		t.Fatalf("runs after report failure = %d, want successful fan-out preserved", runs)
	}
	var links int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM github_check_runs`).Scan(&links); err != nil {
		t.Fatalf("count check links: %v", err)
	}
	if links != 0 {
		t.Fatalf("check links after failed create = %d, want none", links)
	}

	stub.checkStatus = 0
	resp = postApp(t, srv, "merge_group", mgDelivery+"-report-retry", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retry status = %d, want 202 after reporting recovers; body=%s",
			resp.StatusCode, readBody(t, resp))
	}
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM runs
		WHERE cause = $1
	`, string(domain.CauseMergeGroup)).Scan(&runs); err != nil {
		t.Fatalf("count retry runs: %v", err)
	}
	if runs != 2 {
		t.Fatalf("runs after retry = %d, want no duplicate runs", runs)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM github_check_runs`).Scan(&links); err != nil {
		t.Fatalf("count retry check links: %v", err)
	}
	if links != 2 {
		t.Fatalf("check links after retry = %d, want one per required run", links)
	}
}

func TestGitHubAppMergeGroupPartialFanoutErrorReturns500AndRedeliveryCompletes(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)
	seedMergeGroupPipelines(t, pool)

	ctx := context.Background()
	var failedMaterial uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT m.id
		FROM materials m
		JOIN pipelines p ON p.id = m.pipeline_id
		WHERE p.name = 'path-scoped'
		LIMIT 1
	`).Scan(&failedMaterial); err != nil {
		t.Fatalf("failed material id: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE webhook_test_fail_mod_materials (
			id uuid PRIMARY KEY
		)
	`); err != nil {
		t.Fatalf("create fail table: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO webhook_test_fail_mod_materials (id)
		VALUES ($1)
	`, failedMaterial); err != nil {
		t.Fatalf("seed fail table: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION webhook_test_fail_modification()
		RETURNS trigger AS $$
		BEGIN
			IF EXISTS (
				SELECT 1
				FROM webhook_test_fail_mod_materials
				WHERE id = NEW.material_id
			) THEN
				RAISE EXCEPTION 'test partial fan-out failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql
	`); err != nil {
		t.Fatalf("create fail function: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER webhook_test_fail_modification
		BEFORE INSERT ON modifications
		FOR EACH ROW
		EXECUTE FUNCTION webhook_test_fail_modification()
	`); err != nil {
		t.Fatalf("create fail trigger: %v", err)
	}

	body := mergeGroupBody("checks_requested")
	resp := postApp(t, srv, "merge_group", mgDelivery+"-partial", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on partial fan-out; body=%s",
			resp.StatusCode, readBody(t, resp))
	}
	var runs int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM runs
		WHERE cause = $1
	`, string(domain.CauseMergeGroup)).Scan(&runs); err != nil {
		t.Fatalf("count partial runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("runs after partial failure = %d, want successful pipeline preserved", runs)
	}

	if _, err := pool.Exec(ctx, `DROP TRIGGER webhook_test_fail_modification ON modifications`); err != nil {
		t.Fatalf("drop fail trigger: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION webhook_test_fail_modification()`); err != nil {
		t.Fatalf("drop fail function: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE webhook_test_fail_mod_materials`); err != nil {
		t.Fatalf("drop fail table: %v", err)
	}

	resp = postApp(t, srv, "merge_group", mgDelivery+"-partial-retry", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retry status = %d, want 202; body=%s", resp.StatusCode, readBody(t, resp))
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM runs
		WHERE cause = $1
	`, string(domain.CauseMergeGroup)).Scan(&runs); err != nil {
		t.Fatalf("count retry runs: %v", err)
	}
	if runs != 2 {
		t.Fatalf("runs after retry = %d, want one per eligible pipeline", runs)
	}
}

func TestGitHubAppMergeGroupDestroyedCancelsActiveRuns(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newAppServer(t, s)
	seedMergeGroupPipelines(t, pool)

	body := mergeGroupBody("checks_requested")
	resp := postApp(t, srv, "merge_group", mgDelivery, body, signBody(body))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("checks_requested status = %d, want 202", resp.StatusCode)
	}

	body = mergeGroupBody("destroyed")
	resp = postApp(t, srv, "merge_group", "mg-destroyed", body, signBody(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("destroyed status = %d, want 202; body=%s", resp.StatusCode, readBody(t, resp))
	}
	var got struct {
		Canceled []string `json:"canceled_runs"`
		Reason   string   `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode destroyed response: %v", err)
	}
	if len(got.Canceled) != 2 || got.Reason != "invalidated" {
		t.Fatalf("destroyed response = %+v, want two canceled runs with reason invalidated", got)
	}

	var canceled int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM runs
		WHERE cause = $1 AND status = 'canceled'
	`, string(domain.CauseMergeGroup)).Scan(&canceled); err != nil {
		t.Fatalf("count canceled: %v", err)
	}
	if canceled != 2 {
		t.Fatalf("canceled runs = %d, want 2", canceled)
	}
}
