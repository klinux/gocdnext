package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

const (
	mgCancelRepoURL = "https://github.com/org/repo.git"
	mgCancelBaseRef = "main"
)

func seedMergeGroupPipeline(t *testing.T, pool *pgxpool.Pool, s *store.Store, slug string) (uuid.UUID, uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	fp := domain.GitFingerprint(mgCancelRepoURL, mgCancelBaseRef)
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "ci", Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialGit,
				Fingerprint: fp,
				AutoUpdate:  true,
				Git: &domain.GitMaterial{
					URL: mgCancelRepoURL, Branch: mgCancelBaseRef,
					Events: []string{"pull_request"},
				},
			}},
			Jobs: []domain.Job{{Name: "one", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE pipeline_id=$1`, applied.Pipelines[0].PipelineID).Scan(&materialID); err != nil {
		t.Fatalf("material id: %v", err)
	}
	return applied.Pipelines[0].PipelineID, materialID, fp
}

func seedMergeGroupRun(t *testing.T, pool *pgxpool.Pool, s *store.Store, headSHA, headRef string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	fp := domain.GitFingerprint(mgCancelRepoURL, mgCancelBaseRef)

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
						URL: mgCancelRepoURL, Branch: mgCancelBaseRef,
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

	detail, _ := json.Marshal(map[string]any{
		"mg_head_sha":    headSHA,
		"mg_head_ref":    headRef,
		"mg_base_sha":    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"mg_base_ref":    mgCancelBaseRef,
		"mg_fingerprint": fp,
	})
	res, err := s.CreateOrFindMergeGroupRun(ctx, store.MergeGroupRunInput{
		Fingerprint: fp,
		PipelineID:  pipelineID,
		MaterialID:  materialID,
		Revision:    headSHA,
		Branch:      headRef,
		Provider:    "github",
		Delivery:    "seed-" + headSHA[:8],
		TriggeredBy: "test",
		CauseDetail: detail,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return res.Run.RunID
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

	fp := domain.GitFingerprint(mgCancelRepoURL, mgCancelBaseRef)
	canceled, err := s.CancelMergeGroupRuns(context.Background(), fp,
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

	again, err := s.CancelMergeGroupRuns(context.Background(), fp,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "invalidated")
	if err != nil {
		t.Fatalf("redelivery cancel: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("redelivery canceled = %v, want empty", again)
	}
}

func TestCancelMergeGroupRuns_CancelsOnlyMatchingFingerprint(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	fp := domain.GitFingerprint(mgCancelRepoURL, mgCancelBaseRef)
	otherFP := domain.GitFingerprint("https://github.com/org/other.git", mgCancelBaseRef)
	headSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	target := seedMergeGroupRun(t, pool, s, headSHA, "gh-readonly-queue/main/pr-1-aaaa")
	other := seedMergeGroupRun(t, pool, s, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "gh-readonly-queue/main/pr-2-bbbb")
	otherDetail, _ := json.Marshal(map[string]any{
		"mg_head_sha":    headSHA,
		"mg_head_ref":    "gh-readonly-queue/main/pr-2-bbbb",
		"mg_base_sha":    "cccccccccccccccccccccccccccccccccccccccc",
		"mg_base_ref":    mgCancelBaseRef,
		"mg_fingerprint": otherFP,
	})
	if _, err := pool.Exec(ctx, `UPDATE runs SET cause_detail=$2 WHERE id=$1`, other, otherDetail); err != nil {
		t.Fatalf("retarget other fingerprint: %v", err)
	}

	canceled, err := s.CancelMergeGroupRuns(ctx, fp, headSHA, "invalidated")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(canceled) != 1 || canceled[0] != target {
		t.Fatalf("canceled = %v, want only %s", canceled, target)
	}
	if got := scalarStr(t, pool, `SELECT status FROM runs WHERE id=$1`, other); got != "queued" {
		t.Fatalf("other fingerprint run status = %q, want queued", got)
	}
}

func TestMergeGroupDestroyedTombstoneBlocksLaterCreate(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	pipelineID, materialID, fp := seedMergeGroupPipeline(t, pool, s, "mg-tombstone")
	headSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headRef := "gh-readonly-queue/main/pr-1-aaaa"

	canceled, err := s.CancelMergeGroupRuns(ctx, fp, headSHA, "invalidated before fan-out")
	if err != nil {
		t.Fatalf("destroy before fan-out: %v", err)
	}
	if len(canceled) != 0 {
		t.Fatalf("canceled before fan-out = %v, want empty", canceled)
	}
	destroyed, err := s.MergeGroupDestroyed(ctx, fp, headSHA)
	if err != nil {
		t.Fatalf("destroyed lookup: %v", err)
	}
	if !destroyed {
		t.Fatal("destroyed tombstone was not recorded")
	}
	detail, _ := json.Marshal(map[string]any{
		"mg_head_sha":    headSHA,
		"mg_head_ref":    headRef,
		"mg_base_sha":    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"mg_base_ref":    mgCancelBaseRef,
		"mg_fingerprint": fp,
	})
	_, err = s.CreateOrFindMergeGroupRun(ctx, store.MergeGroupRunInput{
		Fingerprint: fp,
		PipelineID:  pipelineID,
		MaterialID:  materialID,
		Revision:    headSHA,
		Branch:      headRef,
		Provider:    "github",
		Delivery:    "late-checks-requested",
		TriggeredBy: "test",
		CauseDetail: detail,
	})
	if !errors.Is(err, store.ErrMergeGroupDestroyed) {
		t.Fatalf("late create err = %v, want ErrMergeGroupDestroyed", err)
	}
	var runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE cause=$1`, string(domain.CauseMergeGroup)).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("runs = %d, want none after destroyed tombstone", runs)
	}
}

func TestMergeGroupCancelEffectsClaimListAndDone(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	runID := seedMergeGroupRun(t, pool, s,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"gh-readonly-queue/main/pr-1-aaaa")
	fp := domain.GitFingerprint(mgCancelRepoURL, mgCancelBaseRef)
	if _, err := s.CancelMergeGroupRuns(ctx, fp,
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

func TestMergeGroupCancelEffectsIgnoreNonDestroyedCancel(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	runID := seedMergeGroupRun(t, pool, s,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"gh-readonly-queue/main/pr-1-aaaa")
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		SET status='canceled',
		    finished_at=NOW(),
		    cancel_reason='operator canceled',
		    merge_group_cancel_effects_claimed_at=NULL,
		    merge_group_cancel_effects_at=NULL
		WHERE id=$1
	`, runID); err != nil {
		t.Fatalf("operator cancel seed: %v", err)
	}

	pending, err := s.ListPendingMergeGroupCancelEffects(ctx, 10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending non-destroyed cancel = %v, want empty", pending)
	}
	claimed, first, err := s.ClaimMergeGroupCancelEffects(ctx, runID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed || first {
		t.Fatalf("claim non-destroyed = (%v,%v), want false,false", claimed, first)
	}
	if _, canceled, err := s.MergeGroupCanceledRunServiceGeneration(ctx, runID); err != nil || canceled {
		t.Fatalf("generation non-destroyed = canceled:%v err:%v, want false nil", canceled, err)
	}
}

func TestCancelMergeGroupRuns_ConcurrentWithCompletion_NoDeadlock(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	runID, _, stageTest, compile, unit := seedRunningJob(t, pool)
	terminalizeJobAndStage(t, pool, unit, stageTest, "success")
	fp := domain.GitFingerprint(mgCancelRepoURL, mgCancelBaseRef)
	headSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	detail, _ := json.Marshal(map[string]any{
		"mg_head_sha":    headSHA,
		"mg_head_ref":    "gh-readonly-queue/main/pr-1-aaaa",
		"mg_base_sha":    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"mg_base_ref":    mgCancelBaseRef,
		"mg_fingerprint": fp,
	})
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		SET cause=$2, cause_detail=$3, ref=$4
		WHERE id=$1
	`, runID, string(domain.CauseMergeGroup), detail, "gh-readonly-queue/main/pr-1-aaaa"); err != nil {
		t.Fatalf("promote to merge_group: %v", err)
	}
	agentID, attempt := jobAgentAttempt(t, pool, compile)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, errs[0] = s.CompleteJob(ctx, store.CompleteJobInput{
			JobRunID: compile, Status: "success", ExitCode: 0,
			ExpectedAgentID: agentID, ExpectedAttempt: attempt,
		})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = s.CancelMergeGroupRuns(ctx, fp, headSHA, "invalidated")
	}()

	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent completion x merge_group cancel timed out (possible deadlock/stuck lock)")
	}
	for i, err := range errs {
		if isDeadlock(err) {
			t.Fatalf("op %d deadlocked (40P01): %v", i, err)
		}
	}
	if got := scalarStr(t, pool, `SELECT status FROM runs WHERE id=$1`, runID); got != "canceled" && got != "success" {
		t.Errorf("run status = %q, want terminal", got)
	}
}
