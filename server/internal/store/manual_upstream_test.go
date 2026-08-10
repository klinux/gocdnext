package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// A manual trigger of an upstream-driven pipeline (deploy-api, whose only
// trigger is the `build-core` upstream) must resolve to the LATEST successful
// upstream run and inherit its counter + commit — so `deploy.version` templates
// the already-built 1.<counter>.<sha> instead of a contextless 1..<sha>.
func TestTriggerManualRun_ResolvesLatestUpstream(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	coreID, apiID, _, coreMat := seedFanoutProject(t, pool)
	coreRunID, _ := completeUpstreamTestStage(t, pool, coreID, coreMat)

	// completeUpstreamTestStage bypasses the scheduler, so it completes the JOBS
	// but leaves the stage_runs 'queued'. In production the `test` stage is
	// 'success' when the build finishes (that's what fires the fanout); mirror
	// that precondition — the material gates on the stage being green.
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status='success' WHERE run_id=$1 AND name='test'`, coreRunID,
	); err != nil {
		t.Fatalf("mark test stage success: %v", err)
	}

	var wantCounter int64
	if err := pool.QueryRow(ctx, `SELECT counter FROM runs WHERE id = $1`, coreRunID).Scan(&wantCounter); err != nil {
		t.Fatalf("core counter: %v", err)
	}

	res, err := s.TriggerManualRun(ctx, store.TriggerManualRunInput{
		PipelineID:  apiID,
		TriggeredBy: "user:alice",
	})
	if err != nil {
		t.Fatalf("TriggerManualRun: %v", err)
	}

	var cause string
	var detailRaw, revisionsRaw []byte
	if err := pool.QueryRow(ctx,
		`SELECT cause, cause_detail, revisions FROM runs WHERE id = $1`, res.RunID,
	).Scan(&cause, &detailRaw, &revisionsRaw); err != nil {
		t.Fatalf("load run: %v", err)
	}

	// Honestly labelled a manual run — the operator kicked it — while still
	// carrying the upstream context so the deploy marker resolves.
	if cause != "manual" {
		t.Fatalf("cause = %q, want manual", cause)
	}

	var detail struct {
		UpstreamRunID      string `json:"upstream_run_id"`
		UpstreamRunCounter int64  `json:"upstream_run_counter"`
		UpstreamPipeline   string `json:"upstream_pipeline"`
		UpstreamStage      string `json:"upstream_stage"`
		ManualUpstream     bool   `json:"manual_upstream"`
	}
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		t.Fatalf("decode cause_detail: %v", err)
	}
	if detail.UpstreamRunCounter != wantCounter {
		t.Fatalf("upstream_run_counter = %d, want %d (the build's counter)", detail.UpstreamRunCounter, wantCounter)
	}
	if detail.UpstreamRunID != coreRunID.String() {
		t.Fatalf("upstream_run_id = %q, want %s", detail.UpstreamRunID, coreRunID)
	}
	if detail.UpstreamPipeline != "build-core" || detail.UpstreamStage != "test" {
		t.Fatalf("upstream identity = %q/%q, want build-core/test", detail.UpstreamPipeline, detail.UpstreamStage)
	}
	if !detail.ManualUpstream {
		t.Fatal("manual_upstream marker not set")
	}

	// Revisions must carry the BUILD's git commit (not the deploy repo's HEAD),
	// so CI_COMMIT_SHA pairs with the counter above.
	if !strings.Contains(string(revisionsRaw), "abc123abc123abc123abc123abc123abc123abc1") {
		t.Fatalf("revisions missing the build commit: %s", revisionsRaw)
	}
}

// With an upstream material but no successful upstream run yet, a manual trigger
// falls back to a plain manual run (no upstream context) — a standalone
// downstream stays hand-kickable before its upstream lands, matching the
// pre-existing contract. It must NOT invent a counter it cannot have.
func TestTriggerManualRun_UpstreamNoSuccessfulRunFallsBack(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, apiID, _, _ := seedFanoutProject(t, pool)

	res, err := s.TriggerManualRun(ctx, store.TriggerManualRunInput{
		PipelineID:  apiID,
		TriggeredBy: "user:alice",
	})
	if err != nil {
		t.Fatalf("TriggerManualRun: %v", err)
	}

	var cause string
	var detailRaw []byte
	if err := pool.QueryRow(ctx,
		`SELECT cause, cause_detail FROM runs WHERE id = $1`, res.RunID,
	).Scan(&cause, &detailRaw); err != nil {
		t.Fatalf("load run: %v", err)
	}
	if cause != "manual" {
		t.Fatalf("cause = %q, want manual", cause)
	}
	if strings.Contains(string(detailRaw), "upstream_run_counter") {
		t.Fatalf("fallback run must carry no upstream context, got %s", detailRaw)
	}
}
