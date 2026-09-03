package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func seedMergeGroupRun(t *testing.T, pool *pgxpool.Pool, s *store.Store, headSHA, headRef string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	const repoURL = "https://github.com/org/repo.git"
	fp := domain.GitFingerprint(repoURL, "main")

	var pipelineID, materialID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT p.id, m.id
		FROM pipelines p
		JOIN materials m ON m.pipeline_id = p.id
		LIMIT 1
	`).Scan(&pipelineID, &materialID); err != nil {
		applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
			Slug: "mg-cancel", Name: "mg-cancel",
			Pipelines: []*domain.Pipeline{{
				Name: "ci", Stages: []string{"build"},
				Materials: []domain.Material{{
					Type:        domain.MaterialGit,
					Fingerprint: fp,
					AutoUpdate:  true,
					Git: &domain.GitMaterial{
						URL: repoURL, Branch: "main",
						Events: []string{"pull_request"},
					},
				}},
				Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
			}},
		})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		pipelineID = applied.Pipelines[0].PipelineID
		if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
			t.Fatalf("material id: %v", err)
		}
	}

	mod, err := s.InsertModification(ctx, store.Modification{
		MaterialID: materialID,
		Revision:   headSHA,
		Branch:     headRef,
	})
	if err != nil {
		t.Fatalf("insert modification: %v", err)
	}
	detail, _ := json.Marshal(map[string]any{
		"mg_head_sha": headSHA,
		"mg_head_ref": headRef,
		"mg_base_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"mg_base_ref": "main",
	})
	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     pipelineID,
		MaterialID:     materialID,
		ModificationID: mod.ID,
		Revision:       headSHA,
		Branch:         headRef,
		Provider:       "github",
		Delivery:       "seed-" + headSHA[:8],
		TriggeredBy:    "test",
		Cause:          string(domain.CauseMergeGroup),
		CauseDetail:    detail,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return res.RunID
}

func TestCancelMergeGroupRuns_CancelsOnlyMatchingHeadSHA(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	target := seedMergeGroupRun(t, pool, s,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"gh-readonly-queue/main/pr-1-aaaa")
	other := seedMergeGroupRun(t, pool, s,
		"cccccccccccccccccccccccccccccccccccccccc",
		"gh-readonly-queue/main/pr-2-cccc")

	canceled, err := s.CancelMergeGroupRuns(context.Background(),
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "invalidated")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(canceled) != 1 || canceled[0] != target {
		t.Fatalf("canceled = %v, want only %s", canceled, target)
	}

	var targetStatus, otherStatus, origin string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM runs WHERE id=$1`, target).Scan(&targetStatus); err != nil {
		t.Fatalf("target status: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT status FROM runs WHERE id=$1`, other).Scan(&otherStatus); err != nil {
		t.Fatalf("other status: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(cancel_origin, '') FROM job_runs WHERE run_id=$1`, target).Scan(&origin); err != nil {
		t.Fatalf("origin: %v", err)
	}
	if targetStatus != "canceled" || otherStatus != "queued" || origin != string(store.CancelOriginMergeGroup) {
		t.Fatalf("targetStatus=%s otherStatus=%s origin=%s", targetStatus, otherStatus, origin)
	}

	again, err := s.CancelMergeGroupRuns(context.Background(),
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "invalidated")
	if err != nil {
		t.Fatalf("redelivery cancel: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("redelivery canceled = %v, want empty", again)
	}
}

func TestMergeGroupCancelEffectsClaimListAndDone(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	runID := seedMergeGroupRun(t, pool, s,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"gh-readonly-queue/main/pr-1-aaaa")
	if _, err := s.CancelMergeGroupRuns(ctx,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "destroyed"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	pending, err := s.ListPendingMergeGroupCancelEffects(ctx, 10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0] != runID {
		t.Fatalf("pending = %v, want [%s]", pending, runID)
	}

	claimed, first, err := s.ClaimMergeGroupCancelEffects(ctx, runID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed || !first {
		t.Fatalf("claim = (%v,%v), want (true,true)", claimed, first)
	}
	claimed, first, err = s.ClaimMergeGroupCancelEffects(ctx, runID)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed || first {
		t.Fatalf("second claim = (%v,%v), want (false,false)", claimed, first)
	}
	pending, err = s.ListPendingMergeGroupCancelEffects(ctx, 10)
	if err != nil {
		t.Fatalf("list pending after claim: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after claim = %v, want empty until lease expires", pending)
	}

	if err := s.MarkMergeGroupCancelEffectsDone(ctx, runID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	pending, err = s.ListPendingMergeGroupCancelEffects(ctx, 10)
	if err != nil {
		t.Fatalf("list pending after done: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after done = %v, want empty", pending)
	}
}
