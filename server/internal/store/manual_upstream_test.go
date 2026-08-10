package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// A manual trigger of an upstream-driven pipeline (deploy-api, whose only
// trigger is the `build-core` upstream) must resolve to the LATEST successful
// upstream run and inherit its counter + commit + ref — so `deploy.version`
// templates the already-built 1.<counter>.<sha> on the SAME lane, instead of a
// contextless 1..<sha> on ref "".
func TestTriggerManualRun_ResolvesLatestUpstream(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	coreID, apiID, _, coreMat := seedFanoutProject(t, pool)
	coreRunID, _ := completeUpstreamTestStage(t, pool, coreID, coreMat)
	// completeUpstreamTestStage bypasses the scheduler and leaves the stage_runs
	// 'queued'; in production the `test` stage is 'success' when the build
	// finishes (that's what the material gates on). Mirror that precondition.
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status='success' WHERE run_id=$1 AND name='test'`, coreRunID,
	); err != nil {
		t.Fatalf("mark test stage success: %v", err)
	}

	var wantCounter int64
	var buildRef string
	if err := pool.QueryRow(ctx,
		`SELECT counter, COALESCE(ref,'') FROM runs WHERE id = $1`, coreRunID,
	).Scan(&wantCounter, &buildRef); err != nil {
		t.Fatalf("core counter/ref: %v", err)
	}

	res, err := s.TriggerManualRun(ctx, store.TriggerManualRunInput{
		PipelineID: apiID, TriggeredBy: "user:alice",
	})
	if err != nil {
		t.Fatalf("TriggerManualRun: %v", err)
	}

	var cause, runRef string
	var detailRaw, revisionsRaw []byte
	if err := pool.QueryRow(ctx,
		`SELECT cause, cause_detail, revisions, COALESCE(ref,'') FROM runs WHERE id = $1`, res.RunID,
	).Scan(&cause, &detailRaw, &revisionsRaw, &runRef); err != nil {
		t.Fatalf("load run: %v", err)
	}

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

	// Revisions carry the BUILD's git commit (not the deploy repo's HEAD).
	if !strings.Contains(string(revisionsRaw), "abc123abc123abc123abc123abc123abc123abc1") {
		t.Fatalf("revisions missing the build commit: %s", revisionsRaw)
	}
	// Ref (supersede lane #97) matches the build's, not default "".
	if buildRef == "" {
		t.Fatal("precondition: build ref should be non-empty (main)")
	}
	if runRef != buildRef {
		t.Fatalf("ref = %q, want the build's ref %q", runRef, buildRef)
	}
}

// The caller's cause_detail (project cron passes schedule_id / schedule_name /
// expression) must survive the upstream merge — the run carries BOTH.
func TestTriggerManualRun_MergesCallerCauseDetail(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	coreID, apiID, _, coreMat := seedFanoutProject(t, pool)
	coreRunID, _ := completeUpstreamTestStage(t, pool, coreID, coreMat)
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status='success' WHERE run_id=$1 AND name='test'`, coreRunID,
	); err != nil {
		t.Fatalf("mark stage: %v", err)
	}

	res, err := s.TriggerManualRun(ctx, store.TriggerManualRunInput{
		PipelineID:  apiID,
		TriggeredBy: "cron",
		Cause:       "schedule",
		CauseDetail: json.RawMessage(`{"schedule_id":"sched-1","schedule_name":"nightly"}`),
	})
	if err != nil {
		t.Fatalf("TriggerManualRun: %v", err)
	}

	var detailRaw []byte
	if err := pool.QueryRow(ctx, `SELECT cause_detail FROM runs WHERE id=$1`, res.RunID).Scan(&detailRaw); err != nil {
		t.Fatalf("load detail: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal(detailRaw, &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d["schedule_id"] != "sched-1" || d["schedule_name"] != "nightly" {
		t.Fatalf("caller cause_detail lost: %s", detailRaw)
	}
	if _, ok := d["upstream_run_counter"]; !ok {
		t.Fatalf("upstream context missing: %s", detailRaw)
	}
}

// A manual-upstream run carries TWO revisions (git checkout + branchless upstream
// material). Re-running it must pick the git checkout — not the UUID slot, which
// has no modification and dropped rerun into ErrNoModificationForPipeline.
func TestRerunRun_ManualUpstreamRunPicksGitMaterial(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	coreID, apiID, _, coreMat := seedFanoutProject(t, pool)
	coreRunID, _ := completeUpstreamTestStage(t, pool, coreID, coreMat)
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status='success' WHERE run_id=$1 AND name='test'`, coreRunID,
	); err != nil {
		t.Fatalf("mark stage: %v", err)
	}

	// In production the build ran from a real webhook, so the git material has a
	// modification row; RerunRun's GetModificationByKey needs it. (The bypass
	// helper doesn't create one.)
	if _, err := s.InsertModification(ctx, store.Modification{
		MaterialID:  coreMat,
		Revision:    "abc123abc123abc123abc123abc123abc123abc1",
		Branch:      "main",
		CommittedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed modification: %v", err)
	}

	created, err := s.TriggerManualRun(ctx, store.TriggerManualRunInput{
		PipelineID: apiID, TriggeredBy: "user:alice",
	})
	if err != nil {
		t.Fatalf("TriggerManualRun: %v", err)
	}
	// RerunRun only accepts a terminal run.
	if _, err := pool.Exec(ctx, `UPDATE runs SET status='success' WHERE id=$1`, created.RunID); err != nil {
		t.Fatalf("mark run terminal: %v", err)
	}

	rerun, err := s.RerunRun(ctx, store.RerunRunInput{RunID: created.RunID, TriggeredBy: "user:bob"})
	if err != nil {
		t.Fatalf("RerunRun of a manual-upstream run: %v", err)
	}
	if rerun.RunID == created.RunID {
		t.Fatal("rerun should be a fresh run")
	}
}

// A pipeline with MORE THAN ONE upstream material is ambiguous — counters aren't
// comparable across pipelines — so a manual trigger must fall back to a plain
// manual run rather than guess which upstream to deploy.
func TestTriggerManualRun_MultipleUpstreamsFallsBack(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	buildMat := func(name string) domain.Material {
		url := "https://example.com/" + name
		return domain.Material{
			Type: domain.MaterialGit, Fingerprint: domain.GitFingerprint(url, "main"), AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
		}
	}
	upstreamMat := func(name string) domain.Material {
		return domain.Material{
			Type: domain.MaterialUpstream, Fingerprint: domain.UpstreamFingerprint(name, "test"), AutoUpdate: true,
			Upstream: &domain.UpstreamMaterial{Pipeline: name, Stage: "test", Status: "success"},
		}
	}
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "multi", Name: "Multi",
		Pipelines: []*domain.Pipeline{
			{Name: "build-a", Stages: []string{"test"}, Materials: []domain.Material{buildMat("build-a")},
				Jobs: []domain.Job{{Name: "t", Stage: "test", Tasks: []domain.Task{{Script: "make"}}}}},
			{Name: "build-b", Stages: []string{"test"}, Materials: []domain.Material{buildMat("build-b")},
				Jobs: []domain.Job{{Name: "t", Stage: "test", Tasks: []domain.Task{{Script: "make"}}}}},
			{Name: "fan-in", Stages: []string{"deploy"},
				Materials: []domain.Material{upstreamMat("build-a"), upstreamMat("build-b")},
				Jobs:      []domain.Job{{Name: "deploy", Stage: "deploy", Tasks: []domain.Task{{Script: "echo"}}}}},
		},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var fanInID uuid.UUID
	for _, p := range applied.Pipelines {
		if p.Name == "fan-in" {
			fanInID = p.PipelineID
		}
	}

	res, err := s.TriggerManualRun(ctx, store.TriggerManualRunInput{
		PipelineID: fanInID, TriggeredBy: "user:alice",
	})
	if err != nil {
		t.Fatalf("TriggerManualRun (multi-upstream): %v", err)
	}
	var detailRaw []byte
	if err := pool.QueryRow(ctx, `SELECT cause_detail FROM runs WHERE id=$1`, res.RunID).Scan(&detailRaw); err != nil {
		t.Fatalf("load detail: %v", err)
	}
	if strings.Contains(string(detailRaw), "upstream_run_counter") {
		t.Fatalf("multi-upstream must fall back to a plain manual run, got upstream context: %s", detailRaw)
	}
}
